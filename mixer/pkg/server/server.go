// Copyright 2017 Istio Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package server

import (
	"fmt"
	"io"
	"net"
	"strings"

	grpc_middleware "github.com/grpc-ecosystem/go-grpc-middleware"
	grpc_prometheus "github.com/grpc-ecosystem/go-grpc-prometheus"
	"github.com/grpc-ecosystem/grpc-opentracing/go/otgrpc"
	ot "github.com/opentracing/opentracing-go"
	"google.golang.org/grpc"

	mixerpb "istio.io/api/mixer/v1"
	"istio.io/istio/mixer/pkg/adapter"
	"istio.io/istio/mixer/pkg/api"
	"istio.io/istio/mixer/pkg/config"
	"istio.io/istio/mixer/pkg/config/store"
	"istio.io/istio/mixer/pkg/pool"
	"istio.io/istio/mixer/pkg/runtime"
	"istio.io/istio/mixer/pkg/runtime/dispatcher"
	"istio.io/istio/mixer/pkg/template"
	"istio.io/istio/pkg/ctrlz"
	"istio.io/istio/pkg/log"
	"istio.io/istio/pkg/probe"
	"istio.io/istio/pkg/tracing"
)

// Server is an in-memory Mixer service.
type Server struct {
	shutdown  chan error
	server    *grpc.Server
	gp        *pool.GoroutinePool //API worker线程池
	adapterGP *pool.GoroutinePool //adapter worker线程池
	listener  net.Listener
	monitor   *monitor
	tracer    io.Closer

	dispatcher dispatcher.Dispatcher

	// probes
	livenessProbe  probe.Controller
	readinessProbe probe.Controller
	*probe.Probe
}

type listenFunc func(network string, address string) (net.Listener, error)

// replaceable set of functions for fault injection
type patchTable struct {
	newRuntime func(s store.Store, templates map[string]*template.Info, adapters map[string]*adapter.Info,
		identityAttribute string, defaultConfigNamespace string, executorPool *pool.GoroutinePool,
		handlerPool *pool.GoroutinePool, enableTracing bool) *runtime.Runtime
	configTracing func(serviceName string, options *tracing.Options) (io.Closer, error)
	startMonitor  func(port uint16, enableProfiling bool, lf listenFunc) (*monitor, error)
	listen        listenFunc
	configLog     func(options *log.Options) error
	runtimeListen func(runtime *runtime.Runtime) error
}

// New instantiates a fully functional Mixer server, ready for traffic.
func New(a *Args) (*Server, error) {
	return newServer(a, newPatchTable())
}

func newPatchTable() *patchTable {
	return &patchTable{
		newRuntime:    runtime.New,
		configTracing: tracing.Configure,
		startMonitor:  startMonitor,
		listen:        net.Listen,
		configLog:     log.Configure,
		runtimeListen: func(rt *runtime.Runtime) error { return rt.StartListening() },
	}
}

func newServer(a *Args, p *patchTable) (*Server, error) {
	if err := a.validate(); err != nil {
		return nil, err
	}

	if err := p.configLog(a.LoggingOptions); err != nil {
		return nil, err
	}

	apiPoolSize := a.APIWorkerPoolSize
	adapterPoolSize := a.AdapterWorkerPoolSize

	s := &Server{}
	// 1.初始化API worker线程池
	s.gp = pool.NewGoroutinePool(apiPoolSize, a.SingleThreaded)
	s.gp.AddWorkers(apiPoolSize)
	// 2.初始化adapter worker线程池
	s.adapterGP = pool.NewGoroutinePool(adapterPoolSize, a.SingleThreaded)
	s.adapterGP.AddWorkers(adapterPoolSize)
	// 3.构造存放Mixer模板仓库
	tmplRepo := template.NewRepository(a.Templates)
	// 4.构造存放adapter的map
	adapterMap := config.AdapterInfoMap(a.Adapters, tmplRepo.SupportsTemplate)

	s.Probe = probe.NewProbe()

	// construct the gRPC options

	var grpcOptions []grpc.ServerOption
	grpcOptions = append(grpcOptions, grpc.MaxConcurrentStreams(uint32(a.MaxConcurrentStreams)))
	grpcOptions = append(grpcOptions, grpc.MaxMsgSize(int(a.MaxMessageSize)))

	var interceptors []grpc.UnaryServerInterceptor
	var err error

	if a.TracingOptions.TracingEnabled() {
		s.tracer, err = p.configTracing("istio-mixer", a.TracingOptions)
		if err != nil {
			_ = s.Close()
			return nil, fmt.Errorf("unable to setup tracing")
		}
		interceptors = append(interceptors, otgrpc.OpenTracingServerInterceptor(ot.GlobalTracer()))
	}

	// setup server prometheus monitoring (as final interceptor in chain)
	interceptors = append(interceptors, grpc_prometheus.UnaryServerInterceptor)
	grpc_prometheus.EnableHandlingTimeHistogram()
	grpcOptions = append(grpcOptions, grpc.UnaryInterceptor(grpc_middleware.ChainUnaryServer(interceptors...)))

	if s.monitor, err = p.startMonitor(a.MonitoringPort, a.EnableProfiling, p.listen); err != nil {
		_ = s.Close()
		return nil, fmt.Errorf("unable to setup monitoring: %v", err)
	}

	// get the network stuff setup
	network := "tcp"
	address := fmt.Sprintf(":%d", a.APIPort)
	if a.APIAddress != "" {
		idx := strings.Index(a.APIAddress, "://")
		if idx < 0 {
			address = a.APIAddress
		} else {
			network = a.APIAddress[:idx]
			address = a.APIAddress[idx+3:]
		}
	}

	if s.listener, err = p.listen(network, address); err != nil {
		_ = s.Close()
		return nil, fmt.Errorf("unable to listen: %v", err)
	}

	st := a.ConfigStore
	if st != nil && a.ConfigStoreURL != "" {
		_ = s.Close()
		return nil, fmt.Errorf("invalid arguments: both ConfigStore and ConfigStoreURL are specified")
	}

	if st == nil {
		configStoreURL := a.ConfigStoreURL
		if configStoreURL == "" {
			configStoreURL = "k8s://"
		}

		reg := store.NewRegistry(config.StoreInventory()...)
		if st, err = reg.NewStore(configStoreURL); err != nil {
			_ = s.Close()
			return nil, fmt.Errorf("unable to connect to the configuration server: %v", err)
		}
	}

	var rt *runtime.Runtime
	templateMap := make(map[string]*template.Info, len(a.Templates))
	for k, v := range a.Templates {
		t := v // Make a local copy, otherwise we end up capturing the location of the last entry
		templateMap[k] = &t
	}
	// 5.构造Mixer runtime实例。runtime实例是Mixer运行时环境的主要入口。
	// 它会监听配置变更，配置变更时会动态构造新的handler实例和dispatcher实例。
	// dispatcher会基于配置和attributes对请求进行调度，调用相应的adapters处理请求。
	rt = p.newRuntime(st, templateMap, adapterMap, a.ConfigIdentityAttribute, a.ConfigDefaultNamespace,
		s.gp, s.adapterGP, a.TracingOptions.TracingEnabled())
	// runtime实例开始监听配置变更，一旦配置变更，runtime实例会构造新的dispatcher。
	if err = p.runtimeListen(rt); err != nil {
		_ = s.Close()
		return nil, fmt.Errorf("unable to listen: %v", err)
	}
	s.dispatcher = rt.Dispatcher()

	// get the grpc server wired up
	grpc.EnableTracing = a.EnableGRPCTracing
	s.server = grpc.NewServer(grpcOptions...)
	// 6.注册Mixer gRPC server
	mixerpb.RegisterMixerServer(s.server, api.NewGRPCServer(s.dispatcher, s.gp))

	if a.LivenessProbeOptions.IsValid() {
		s.livenessProbe = probe.NewFileController(a.LivenessProbeOptions)
		s.RegisterProbe(s.livenessProbe, "server")
		s.livenessProbe.Start()
	}

	if a.ReadinessProbeOptions.IsValid() {
		s.readinessProbe = probe.NewFileController(a.ReadinessProbeOptions)
		rt.RegisterProbe(s.readinessProbe, "dispatcher")
		st.RegisterProbe(s.readinessProbe, "store")
		s.readinessProbe.Start()
	}
	// 启动ControlZ监听器，ControlZ提供了Istio的内省功能。Mixer与ctrlz集成时，会启动一个
	// web service监听器用于展示Mixer的环境变量、参数版本信息、内存信息、进程信息、metrics等。
	go ctrlz.Run(a.IntrospectionOptions, nil)

	return s, nil
}

// Run enables Mixer to start receiving gRPC requests on its main API port.
func (s *Server) Run() {
	s.shutdown = make(chan error, 1)
	s.SetAvailable(nil)
	go func() {
		// go to work...
		err := s.server.Serve(s.listener)

		// notify closer we're done
		s.shutdown <- err
	}()
}

// Wait waits for the server to exit.
func (s *Server) Wait() error {
	if s.shutdown == nil {
		return fmt.Errorf("server not running")
	}

	err := <-s.shutdown
	s.shutdown = nil
	return err
}

// Close cleans up resources used by the server.
func (s *Server) Close() error {
	if s.shutdown != nil {
		s.server.GracefulStop()
		_ = s.Wait()
	}

	if s.listener != nil {
		_ = s.listener.Close()
	}

	if s.tracer != nil {
		_ = s.tracer.Close()
	}

	if s.monitor != nil {
		_ = s.monitor.Close()
	}

	if s.gp != nil {
		_ = s.gp.Close()
	}

	if s.adapterGP != nil {
		_ = s.adapterGP.Close()
	}

	if s.livenessProbe != nil {
		_ = s.livenessProbe.Close()
	}

	if s.readinessProbe != nil {
		_ = s.readinessProbe.Close()
	}

	// final attempt to purge buffered logs
	_ = log.Sync()

	return nil
}

// Addr returns the address of the server's API port, where gRPC requests can be sent.
func (s *Server) Addr() net.Addr {
	return s.listener.Addr()
}

// Dispatcher returns the dispatcher that was created during server creation. This should only
// be used for testing purposes only.
func (s *Server) Dispatcher() dispatcher.Dispatcher {
	return s.dispatcher
}
