package main

import (
	"context"
	"flag"
	"fmt"
	"github.com/hashicorp/consul/api"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/go-kit/kit/endpoint"
	"github.com/go-kit/kit/log"
	"github.com/go-kit/kit/sd"
	consulsd "github.com/go-kit/kit/sd/consul"
	"github.com/go-kit/kit/sd/lb"
	httptransport "github.com/go-kit/kit/transport/http"
	"github.com/gorilla/mux"

	"github.com/pascallin/go-micro-services/pkg/addsvc/addtransport"
	"github.com/pascallin/go-micro-services/pkg/stringsvc"
)

func main() {
	var (
		httpAddr     = flag.String("http.addr", ":8000", "Address for HTTP (JSON) server")
		consulAddr   = flag.String("consul.addr", ":8500", "Consul agent address")
		retryMax     = flag.Int("retry.max", 3, "per-request retries to different instances")
		retryTimeout = flag.Duration("retry.timeout", 500*time.Millisecond, "per-request timeout, including retries")
	)
	flag.Parse()

	// Logging domain.
	var logger log.Logger
	{
		logger = log.NewLogfmtLogger(os.Stderr)
		logger = log.With(logger, "ts", log.DefaultTimestampUTC)
		logger = log.With(logger, "caller", log.DefaultCaller)
	}

	// Service discovery domain. In this example we use Consul.
	var client consulsd.Client
	{
		consulConfig := api.DefaultConfig()
		if len(*consulAddr) > 0 {
			consulConfig.Address = *consulAddr
		}
		consulClient, err := api.NewClient(consulConfig)
		if err != nil {
			logger.Log("err", err)
			os.Exit(1)
		}
		client = consulsd.NewClient(consulClient)
	}

	// Transport domain.
	//tracer := stdopentracing.GlobalTracer() // no-op
	//zipkinTracer, _ := stdzipkin.NewTracer(nil, stdzipkin.WithNoopTracer(true))
	r := mux.NewRouter()
	ctx := context.Background()
	// Now we begin installing the routes. Each route corresponds to a single
	// method: sum, concat, uppercase, and count.

	// strsvc routes.
	{
		// Each method gets constructed with a factory. Factories take an
		// instance string, and return a specific endpoint. In the factory we
		// dial the instance string we get from Consul, and then leverage an
		// addsvc client package to construct a complete service. We can then
		// leverage the addsvc.Make{Sum,Concat}Endpoint constructors to convert
		// the complete service to specific endpoint.
		var (
			tags        = []string{}
			passingOnly = true
			sum   		endpoint.Endpoint
			concat		endpoint.Endpoint
			instancer   = consulsd.NewInstancer(client, logger, "addsvc", tags, passingOnly)
		)
		{
			factory := addsvcFactory(ctx, "POST", "/sum")
			endpointer := sd.NewEndpointer(instancer, factory, logger)
			balancer := lb.NewRoundRobin(endpointer)
			retry := lb.Retry(*retryMax, *retryTimeout, balancer)
			sum = retry
		}
		{
			factory := addsvcFactory(ctx, "POST", "/concat")
			endpointer := sd.NewEndpointer(instancer, factory, logger)
			balancer := lb.NewRoundRobin(endpointer)
			retry := lb.Retry(*retryMax, *retryTimeout, balancer)
			concat = retry
		}

		r.Handle("/addsvc/sum", httptransport.NewServer(sum, addtransport.DecodeHTTPSumRequest, addtransport.EncodeHTTPGenericResponse))
		r.Handle("/addsvc/concat", httptransport.NewServer(concat, addtransport.DecodeHTTPConcatRequest, addtransport.EncodeHTTPGenericResponse))
	}

	// stringsvc routes
	{
		var (
			tags        = []string{}
			passingOnly = true
			uppercase   		endpoint.Endpoint
			count		endpoint.Endpoint
			instancer   = consulsd.NewInstancer(client, logger, "addstring", tags, passingOnly)
		)
		{
			factory := stringsvcFactory(ctx, "POST", "/uppercase")
			endpointer := sd.NewEndpointer(instancer, factory, logger)
			balancer := lb.NewRoundRobin(endpointer)
			retry := lb.Retry(*retryMax, *retryTimeout, balancer)
			uppercase = retry
		}
		{
			factory := stringsvcFactory(ctx, "POST", "/count")
			endpointer := sd.NewEndpointer(instancer, factory, logger)
			balancer := lb.NewRoundRobin(endpointer)
			retry := lb.Retry(*retryMax, *retryTimeout, balancer)
			count = retry
		}

		r.Handle("/stringsvc/uppercase", httptransport.NewServer(uppercase, stringsvc.DecodeUppercaseRequest, stringsvc.EncodeResponse))
		r.Handle("/stringsvc/count", httptransport.NewServer(count, stringsvc.DecodeUppercaseRequest, stringsvc.EncodeResponse))
	}

	// Interrupt handler.
	errc := make(chan error)
	go func() {
		c := make(chan os.Signal)
		signal.Notify(c, syscall.SIGINT, syscall.SIGTERM)
		errc <- fmt.Errorf("%s", <-c)
	}()

	// HTTP strtransport.
	go func() {
		logger.Log("strtransport", "HTTP", "addr", *httpAddr)
		errc <- http.ListenAndServe(*httpAddr, r)
	}()

	// Run!
	logger.Log("exit", <-errc)
}

func addsvcFactory(ctx context.Context, method, path string) sd.Factory {
	return func(instance string) (endpoint.Endpoint, io.Closer, error) {
		if !strings.HasPrefix(instance, "http") {
			instance = "http://" + instance
		}
		tgt, err := url.Parse(instance)
		if err != nil {
			return nil, nil, err
		}
		tgt.Path = path

		var (
			enc httptransport.EncodeRequestFunc
			dec httptransport.DecodeResponseFunc
		)
		switch path {
		case "/sum":
			enc, dec = addtransport.EncodeHTTPGenericRequest,  addtransport.DecodeHTTPSumResponse
		case "/concat":
			enc, dec =  addtransport.EncodeHTTPGenericRequest,  addtransport.DecodeHTTPConcatResponse
		default:
			return nil, nil, fmt.Errorf("unknown stringsvc path %q", path)
		}

		return httptransport.NewClient(method, tgt, enc, dec).Endpoint(), nil, nil
	}
}

func stringsvcFactory(ctx context.Context, method, path string) sd.Factory {
	return func(instance string) (endpoint.Endpoint, io.Closer, error) {
		if !strings.HasPrefix(instance, "http") {
			instance = "http://" + instance
		}
		tgt, err := url.Parse(instance)
		if err != nil {
			return nil, nil, err
		}
		tgt.Path = path

		var (
			enc httptransport.EncodeRequestFunc
			dec httptransport.DecodeResponseFunc
		)
		switch path {
		case "/uppercase":
			enc, dec = stringsvc.EncodeRequest,  stringsvc.DecodeUppercaseResponse
		case "/count":
			enc, dec = stringsvc.EncodeRequest,  stringsvc.DecodeCountResponse
		default:
			return nil, nil, fmt.Errorf("unknown stringsvc path %q", path)
		}

		return httptransport.NewClient(method, tgt, enc, dec).Endpoint(), nil, nil
	}
}