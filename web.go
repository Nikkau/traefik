package main

import (
	"encoding/json"
	"expvar"
	"fmt"
	"io/ioutil"
	"net/http"
	"runtime"

	"github.com/codegangsta/negroni"
	"github.com/containous/traefik/autogen"
	"github.com/containous/traefik/log"
	"github.com/containous/traefik/middlewares"
	"github.com/containous/traefik/safe"
	"github.com/containous/traefik/types"
	"github.com/containous/traefik/version"
	"github.com/elazarl/go-bindata-assetfs"
	"github.com/pressly/chi"
	"github.com/thoas/stats"
	"github.com/unrolled/render"
)

var metrics = stats.New()

// WebProvider is a provider.Provider implementation that provides the UI.
// FIXME to be handled another way.
type WebProvider struct {
	Address  string `description:"Web administration port"`
	CertFile string `description:"SSL certificate"`
	KeyFile  string `description:"SSL certificate"`
	ReadOnly bool   `description:"Enable read only API"`
	server   *Server
	Auth     *types.Auth
}

var (
	templatesRenderer = render.New(render.Options{
		Directory: "nowhere",
	})
)

func init() {
	expvar.Publish("Goroutines", expvar.Func(goroutines))
}

func goroutines() interface{} {
	return runtime.NumGoroutine()
}

// Provide allows the provider to provide configurations to traefik
// using the given configuration channel.
func (provider *WebProvider) Provide(configurationChan chan<- types.ConfigMessage, pool *safe.Pool, _ []types.Constraint) error {

	systemRouter := chi.NewRouter()

	// health route
	systemRouter.Get("/health", provider.getHealthHandler)

	// ping route
	systemRouter.Get("/ping", provider.getPingHandler)
	// API routes
	systemRouter.Get("/api", provider.getConfigHandler)
	systemRouter.Get("/api/version", provider.getVersionHandler)

	systemRouter.Get("/api/providers", provider.getConfigHandler)

	systemRouter.Route("/api/providers", func(r chi.Router) {
		r.Get("/:provider", provider.getProviderHandler)
		r.Put("/:provider", provider.makePutProviderHandler(configurationChan))

		r.Route("/:provider", func(r chi.Router) {
			r.Get("/backends", provider.getBackendsHandler)

			r.Route("/backends", func(r chi.Router) {
				r.Get("/:backend", provider.getBackendHandler)
				r.Get("/:backend/servers", provider.getServersHandler)
				r.Get("/:backend/servers/:server", provider.getServerHandler)
			})

			r.Get("/frontends", provider.getFrontendsHandler)

			r.Route("/frontends", func(r chi.Router) {
				r.Get("/:frontend", provider.getFrontendHandler)
				r.Get("/:frontend/routes", provider.getRoutesHandler)
				r.Get("/:frontend/routes/:route", provider.getRouteHandler)
			})
		})
	})

	// Expose dashboard
	systemRouter.Get("/", func(response http.ResponseWriter, request *http.Request) {
		http.Redirect(response, request, "/dashboard/", 302)
	})
	systemRouter.FileServer("/dashboard/", &assetfs.AssetFS{Asset: autogen.Asset, AssetInfo: autogen.AssetInfo, AssetDir: autogen.AssetDir, Prefix: "static"})

	// expvars
	if provider.server.globalConfiguration.Debug {
		systemRouter.Get("/debug/vars", expvarHandler)
	}

	go func() {
		var err error
		var negroni = negroni.New()
		if provider.Auth != nil {
			authMiddleware, err := middlewares.NewAuthenticator(provider.Auth)
			if err != nil {
				log.Fatal("Error creating Auth: ", err)
			}
			negroni.Use(authMiddleware)
		}
		negroni.UseHandler(systemRouter)

		if len(provider.CertFile) > 0 && len(provider.KeyFile) > 0 {
			err = http.ListenAndServeTLS(provider.Address, provider.CertFile, provider.KeyFile, negroni)
		} else {
			err = http.ListenAndServe(provider.Address, negroni)
		}

		if err != nil {
			log.Fatal("Error creating server: ", err)
		}
	}()
	return nil
}

func (provider *WebProvider) getHealthHandler(response http.ResponseWriter, request *http.Request) {
	templatesRenderer.JSON(response, http.StatusOK, metrics.Data())
}

func (provider *WebProvider) getPingHandler(response http.ResponseWriter, request *http.Request) {
	response.Write([]byte("OK"))
}

func (provider *WebProvider) getConfigHandler(response http.ResponseWriter, request *http.Request) {
	currentConfigurations := provider.server.currentConfigurations.Get().(configs)
	templatesRenderer.JSON(response, http.StatusOK, currentConfigurations)
}

func (provider *WebProvider) getVersionHandler(response http.ResponseWriter, request *http.Request) {
	v := struct {
		Version  string
		Codename string
	}{
		Version:  version.Version,
		Codename: version.Codename,
	}
	templatesRenderer.JSON(response, http.StatusOK, v)
}

func (provider *WebProvider) getProviderHandler(response http.ResponseWriter, request *http.Request) {
	providerID := chi.URLParam(request, "provider")
	currentConfigurations := provider.server.currentConfigurations.Get().(configs)
	if provider, ok := currentConfigurations[providerID]; ok {
		templatesRenderer.JSON(response, http.StatusOK, provider)
	} else {
		http.NotFound(response, request)
	}
}

func (provider *WebProvider) makePutProviderHandler(configurationChan chan<- types.ConfigMessage) func(response http.ResponseWriter, request *http.Request) {
	return func(response http.ResponseWriter, request *http.Request) {
		if provider.ReadOnly {
			response.WriteHeader(http.StatusForbidden)
			fmt.Fprintf(response, "REST API is in read-only mode")
			return
		}
		if chi.URLParam(request, "provider") != "web" {
			response.WriteHeader(http.StatusBadRequest)
			fmt.Fprintf(response, "Only 'web' provider can be updated through the REST API")
			return
		}

		configuration := new(types.Configuration)
		body, _ := ioutil.ReadAll(request.Body)
		err := json.Unmarshal(body, configuration)
		if err == nil {
			configurationChan <- types.ConfigMessage{ProviderName: "web", Configuration: configuration}
			provider.getConfigHandler(response, request)
		} else {
			log.Errorf("Error parsing configuration %+v", err)
			http.Error(response, fmt.Sprintf("%+v", err), http.StatusBadRequest)
		}
	}
}

func (provider *WebProvider) getBackendsHandler(response http.ResponseWriter, request *http.Request) {
	providerID := chi.URLParam(request, "provider")
	currentConfigurations := provider.server.currentConfigurations.Get().(configs)
	if provider, ok := currentConfigurations[providerID]; ok {
		templatesRenderer.JSON(response, http.StatusOK, provider.Backends)
	} else {
		http.NotFound(response, request)
	}
}

func (provider *WebProvider) getBackendHandler(response http.ResponseWriter, request *http.Request) {
	providerID := chi.URLParam(request, "provider")
	backendID := chi.URLParam(request, "backend")
	currentConfigurations := provider.server.currentConfigurations.Get().(configs)
	if provider, ok := currentConfigurations[providerID]; ok {
		if backend, ok := provider.Backends[backendID]; ok {
			templatesRenderer.JSON(response, http.StatusOK, backend)
			return
		}
	}
	http.NotFound(response, request)
}

func (provider *WebProvider) getServersHandler(response http.ResponseWriter, request *http.Request) {
	providerID := chi.URLParam(request, "provider")
	backendID := chi.URLParam(request, "backend")
	currentConfigurations := provider.server.currentConfigurations.Get().(configs)
	if provider, ok := currentConfigurations[providerID]; ok {
		if backend, ok := provider.Backends[backendID]; ok {
			templatesRenderer.JSON(response, http.StatusOK, backend.Servers)
			return
		}
	}
	http.NotFound(response, request)
}

func (provider *WebProvider) getServerHandler(response http.ResponseWriter, request *http.Request) {
	providerID := chi.URLParam(request, "provider")
	backendID := chi.URLParam(request, "backend")
	serverID := chi.URLParam(request, "server")
	currentConfigurations := provider.server.currentConfigurations.Get().(configs)
	if provider, ok := currentConfigurations[providerID]; ok {
		if backend, ok := provider.Backends[backendID]; ok {
			if server, ok := backend.Servers[serverID]; ok {
				templatesRenderer.JSON(response, http.StatusOK, server)
				return
			}
		}
	}
	http.NotFound(response, request)
}

func (provider *WebProvider) getFrontendsHandler(response http.ResponseWriter, request *http.Request) {
	providerID := chi.URLParam(request, "provider")
	currentConfigurations := provider.server.currentConfigurations.Get().(configs)
	if provider, ok := currentConfigurations[providerID]; ok {
		templatesRenderer.JSON(response, http.StatusOK, provider.Frontends)
	} else {
		http.NotFound(response, request)
	}
}

func (provider *WebProvider) getFrontendHandler(response http.ResponseWriter, request *http.Request) {
	providerID := chi.URLParam(request, "provider")
	frontendID := chi.URLParam(request, "frontend")
	currentConfigurations := provider.server.currentConfigurations.Get().(configs)
	if provider, ok := currentConfigurations[providerID]; ok {
		if frontend, ok := provider.Frontends[frontendID]; ok {
			templatesRenderer.JSON(response, http.StatusOK, frontend)
			return
		}
	}
	http.NotFound(response, request)
}

func (provider *WebProvider) getRoutesHandler(response http.ResponseWriter, request *http.Request) {
	providerID := chi.URLParam(request, "provider")
	frontendID := chi.URLParam(request, "frontend")
	currentConfigurations := provider.server.currentConfigurations.Get().(configs)
	if provider, ok := currentConfigurations[providerID]; ok {
		if frontend, ok := provider.Frontends[frontendID]; ok {
			templatesRenderer.JSON(response, http.StatusOK, frontend.Routes)
			return
		}
	}
	http.NotFound(response, request)
}

func (provider *WebProvider) getRouteHandler(response http.ResponseWriter, request *http.Request) {
	providerID := chi.URLParam(request, "provider")
	frontendID := chi.URLParam(request, "frontend")
	routeID := chi.URLParam(request, "route")
	currentConfigurations := provider.server.currentConfigurations.Get().(configs)
	if provider, ok := currentConfigurations[providerID]; ok {
		if frontend, ok := provider.Frontends[frontendID]; ok {
			if route, ok := frontend.Routes[routeID]; ok {
				templatesRenderer.JSON(response, http.StatusOK, route)
				return
			}
		}
	}
	http.NotFound(response, request)
}

func expvarHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	fmt.Fprintf(w, "{\n")
	first := true
	expvar.Do(func(kv expvar.KeyValue) {
		if !first {
			fmt.Fprintf(w, ",\n")
		}
		first = false
		fmt.Fprintf(w, "%q: %s", kv.Key, kv.Value)
	})
	fmt.Fprintf(w, "\n}\n")
}
