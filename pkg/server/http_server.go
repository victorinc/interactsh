package server

import (
	"bytes"
	"crypto/tls"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"strings"
	"time"

	"server/pkg/server/acme"

	jsoniter "github.com/json-iterator/go"
	"github.com/projectdiscovery/gologger"
	"github.com/projectdiscovery/gologger/levels"
)

// HTTPServer is a http server instance that listens both
// TLS and Non-TLS based servers.
type HTTPServer struct {
	options      *Options
	domain       string
	tlsserver    http.Server
	nontlsserver http.Server
}

type noopLogger struct {
}

func (l *noopLogger) Write(p []byte) (n int, err error) {
	return 0, nil
}

// NewHTTPServer returns a new TLS & Non-TLS HTTP server.
func NewHTTPServer(options *Options) (*HTTPServer, error) {
	gologger.DefaultLogger.SetMaxLevel(levels.LevelDebug)

	server := &HTTPServer{options: options, domain: strings.TrimSuffix(options.Domain, ".")}

	router := &http.ServeMux{}
	router.Handle("/", server.logger(http.HandlerFunc(server.defaultHandler)))
	router.Handle("/register", server.corsMiddleware(server.authMiddleware(http.HandlerFunc(server.registerHandler))))
	router.Handle("/deregister", server.corsMiddleware(server.authMiddleware(http.HandlerFunc(server.deregisterHandler))))
	router.Handle("/poll", server.corsMiddleware(server.authMiddleware(http.HandlerFunc(server.pollHandler))))
	router.Handle("/metrics", server.corsMiddleware(server.authMiddleware(http.HandlerFunc(server.metricsHandler))))
	server.tlsserver = http.Server{Addr: options.ListenIP + ":443", Handler: router, ErrorLog: log.New(&noopLogger{}, "", 0)}
	server.nontlsserver = http.Server{Addr: options.ListenIP + ":80", Handler: router, ErrorLog: log.New(&noopLogger{}, "", 0)}
	return server, nil
}

// ListenAndServe listens on http and/or https ports for the server.
func (h *HTTPServer) ListenAndServe(autoTLS *acme.AutoTLS) {
	go func() {
		if autoTLS == nil {
			return
		}
		h.tlsserver.TLSConfig = &tls.Config{}
		h.tlsserver.TLSConfig.GetCertificate = autoTLS.GetCertificateFunc()

		if err := h.tlsserver.ListenAndServeTLS("", ""); err != nil {
			gologger.Error().Msgf("Could not serve http on tls: %s\n", err)
		}
	}()

	if err := h.nontlsserver.ListenAndServe(); err != nil {
		gologger.Error().Msgf("Could not serve http: %s\n", err)
	}
}

func (h *HTTPServer) logger(handler http.Handler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		req, _ := httputil.DumpRequest(r, true)
		reqString := string(req)

		gologger.Debug().Msgf("New HTTP request: %s\n", reqString)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, r)

		resp, _ := httputil.DumpResponse(rec.Result(), true)
		resoString := string(resp)

		for k, v := range rec.Header() {
			w.Header()[k] = v
		}
		data := rec.Body.Bytes()

		w.WriteHeader(rec.Result().StatusCode)
		_, _ = w.Write(data)

		// if root-tld is enabled stores any interaction towards the main domain
		if h.options.RootTLD && strings.HasSuffix(r.Host, h.domain) {
			ID := h.domain
			host, _, _ := net.SplitHostPort(r.RemoteAddr)
			interaction := &Interaction{
				Protocol:      "http",
				UniqueID:      r.Host,
				FullId:        r.Host,
				RawRequest:    reqString,
				RawResponse:   resoString,
				RemoteAddress: host,
				Timestamp:     time.Now(),
			}
			buffer := &bytes.Buffer{}
			if err := jsoniter.NewEncoder(buffer).Encode(interaction); err != nil {
				gologger.Warning().Msgf("Could not encode root tld http interaction: %s\n", err)
			} else {
				gologger.Debug().Msgf("Root TLD HTTP Interaction: \n%s\n", buffer.String())
				if err := h.options.Storage.AddInteractionWithId(ID, buffer.Bytes()); err != nil {
					gologger.Warning().Msgf("Could not store root tld http interaction: %s\n", err)
				}
			}
		}

		var uniqueID, fullID string
		parts := strings.Split(r.Host, ".")
		for i, part := range parts {
			if len(part) == 33 {
				uniqueID = part
				fullID = part
				if i+1 <= len(parts) {
					fullID = strings.Join(parts[:i+1], ".")
				}
			}
		}
		if uniqueID != "" {
			correlationID := uniqueID[:20]

			host, _, _ := net.SplitHostPort(r.RemoteAddr)
			interaction := &Interaction{
				Protocol:      "http",
				UniqueID:      uniqueID,
				FullId:        fullID,
				RawRequest:    reqString,
				RawResponse:   resoString,
				RemoteAddress: host,
				Timestamp:     time.Now(),
			}
			buffer := &bytes.Buffer{}
			if err := jsoniter.NewEncoder(buffer).Encode(interaction); err != nil {
				gologger.Warning().Msgf("Could not encode http interaction: %s\n", err)
			} else {
				gologger.Debug().Msgf("HTTP Interaction: \n%s\n", buffer.String())
				if err := h.options.Storage.AddInteraction(correlationID, buffer.Bytes()); err != nil {
					gologger.Warning().Msgf("Could not store http interaction: %s\n", err)
				}
			}
		}
	}
}

const banner = `<h1> Interactsh Server </h1>

<a href='https://github.com/projectdiscovery/interactsh'>Interactsh</a> is an <b>open-source solution</b> for out-of-band data extraction. It is a tool designed to detect bugs that cause external interactions. These bugs include, Blind SQLi, Blind CMDi, SSRF, etc. <br><br>

If you find communications or exchanges with the <b>%s</b> server in your logs, it is possible that someone has been testing your applications.<br><br>

You should review the time when these interactions were initiated to identify the person responsible for this testing.
`

// defaultHandler is a handler for default collaborator requests
func (h *HTTPServer) defaultHandler(w http.ResponseWriter, req *http.Request) {
	reflection := URLReflection(req.Host)
	w.Header().Set("Server", h.domain)

	if req.URL.Path == "/" && reflection == "" {
		fmt.Fprintf(w, banner, h.domain)
	} else if strings.EqualFold(req.URL.Path, "/robots.txt") {
		fmt.Fprintf(w, "User-agent: *\nDisallow: / # %s", reflection)
	} else if strings.HasSuffix(req.URL.Path, ".json") {
		fmt.Fprintf(w, "{\"data\":\"%s\"}", reflection)
		w.Header().Set("Content-Type", "application/json")
	} else if strings.HasSuffix(req.URL.Path, ".xml") {
		fmt.Fprintf(w, "<data>%s</data>", reflection)
		w.Header().Set("Content-Type", "application/xml")
	} else {
		fmt.Fprintf(w, "<html><head></head><body>%s</body></html>", reflection)
	}
}

// // RegisterRequest is a request for client registration to interactsh server.
// type RegisterRequest struct {
// 	// PublicKey is the public RSA Key of the client.
// 	PublicKey string `json:"public-key"`
// 	// SecretKey is the secret-key for correlation ID registered for the client.
// 	SecretKey string `json:"secret-key"`
// 	// CorrelationID is an ID for correlation with requests.
// 	CorrelationID string `json:"correlation-id"`
// }

type RegisterRequest struct {
	// PublicKey is the public RSA Key of the client.
	Token string `json:"token"`
	// CorrelationID is an ID for correlation with requests.
	SessionID string `json:"session-id"`
}

// registerHandler is a handler for client register requests
func (h *HTTPServer) registerHandler(w http.ResponseWriter, req *http.Request) {
	r := &RegisterRequest{}
	if err := jsoniter.NewDecoder(req.Body).Decode(r); err != nil {
		gologger.Warning().Msgf("Could not decode json body: %s\n", err)
		jsonError(w, fmt.Sprintf("could not decode json body: %s", err), http.StatusBadRequest)
		return
	}
	if err := h.options.Storage.SetIDPublicKey(r.SessionID, r.Token); err != nil {
		gologger.Warning().Msgf("Could not set id and public key for %s: %s\n", r.SessionID, err)
		jsonError(w, fmt.Sprintf("could not set id and public key: %s", err), http.StatusBadRequest)
		return
	}
	jsonMsg(w, "registration successful", http.StatusOK)
	gologger.Debug().Msgf("Registered correlationID %s for key\n", r.SessionID)
}

// DeregisterRequest is a request for client deregistration to interactsh server.
type DeregisterRequest struct {
	// PublicKey is the public RSA Key of the client.
	Token string `json:"token"`
	// CorrelationID is an ID for correlation with requests.
	SessionID string `json:"session-id"`
}

// deregisterHandler is a handler for client deregister requests
func (h *HTTPServer) deregisterHandler(w http.ResponseWriter, req *http.Request) {
	fmt.Sprintf("here in deregister")
	r := &DeregisterRequest{}
	if err := jsoniter.NewDecoder(req.Body).Decode(r); err != nil {
		gologger.Warning().Msgf("Could not decode json body: %s\n", err)
		jsonError(w, fmt.Sprintf("could not decode json body: %s", err), http.StatusBadRequest)
		return
	}
	if err := h.options.Storage.RemoveID(r.SessionID, r.Token); err != nil {
		gologger.Warning().Msgf("Could not remove id for %s: %s\n", r.SessionID, err)
		jsonError(w, fmt.Sprintf("could not remove id: %s", err), http.StatusBadRequest)
		return
	}
	jsonMsg(w, "deregistration successful", http.StatusOK)
	gologger.Debug().Msgf("Deregistered correlationID %s for key\n", r.SessionID)
}

// PollResponse is the response for a polling request
type PollResponse struct {
	Data    []string `json:"data"`
	Extra   []string `json:"extra"`
	AESKey  string   `json:"aes_key"`
	TLDData []string `json:"tlddata,omitempty"`
}

// pollHandler is a handler for client poll requests
func (h *HTTPServer) pollHandler(w http.ResponseWriter, req *http.Request) {
	ID := req.URL.Query().Get("id")
	if ID == "" {
		jsonError(w, "no id specified for poll", http.StatusBadRequest)
		return
	}
	secret := req.URL.Query().Get("secret")
	if secret == "" {
		jsonError(w, "no secret specified for poll", http.StatusBadRequest)
		return
	}

	data, aesKey, err := h.options.Storage.GetInteractions(ID, secret)
	if err != nil {
		gologger.Warning().Msgf("Could not get interactions for %s: %s\n", ID, err)
		jsonError(w, fmt.Sprintf("could not get interactions: %s", err), http.StatusBadRequest)
		return
	}

	// At this point the client is authenticated, so we return also the data related to the auth token
	extradata, _ := h.options.Storage.GetInteractionsWithId(h.options.Token)
	var tlddata []string
	if h.options.RootTLD {
		tlddata, _ = h.options.Storage.GetInteractionsWithId(h.options.Domain)
	}
	response := &PollResponse{Data: data, AESKey: aesKey, TLDData: tlddata, Extra: extradata}

	if err := jsoniter.NewEncoder(w).Encode(response); err != nil {
		gologger.Warning().Msgf("Could not encode interactions for %s: %s\n", ID, err)
		jsonError(w, fmt.Sprintf("could not encode interactions: %s", err), http.StatusBadRequest)
		return
	}
	gologger.Debug().Msgf("Polled %d interactions for %s correlationID\n", len(data), ID)
}

func (h *HTTPServer) corsMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		// Set CORS headers for the preflight request
		if req.Method == http.MethodOptions {
			w.Header().Set("Access-Control-Allow-Origin", h.options.OriginURL)
			w.Header().Set("Access-Control-Allow-Credentials", "true")
			w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization")
			w.WriteHeader(http.StatusNoContent)
			return
		}
		w.Header().Set("Access-Control-Allow-Origin", h.options.OriginURL)
		w.Header().Set("Access-Control-Allow-Credentials", "true")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization")
		next.ServeHTTP(w, req)
	})
}

func jsonBody(w http.ResponseWriter, key, value string, code int) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(code)
	_ = jsoniter.NewEncoder(w).Encode(map[string]interface{}{key: value})
}

func jsonError(w http.ResponseWriter, err string, code int) {
	jsonBody(w, "error", err, code)
}

func jsonMsg(w http.ResponseWriter, err string, code int) {
	jsonBody(w, "message", err, code)
}

func (h *HTTPServer) authMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if !h.checkToken(req) {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, req)
	})
}

func (h *HTTPServer) checkToken(req *http.Request) bool {
	return !h.options.Auth || h.options.Auth && h.options.Token == req.Header.Get("Authorization")
}

// metricsHandler is a handler for /metrics endpoint
func (h *HTTPServer) metricsHandler(w http.ResponseWriter, req *http.Request) {
	metrics := h.options.Storage.GetCacheMetrics()

	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	_ = jsoniter.NewEncoder(w).Encode(metrics)
}
