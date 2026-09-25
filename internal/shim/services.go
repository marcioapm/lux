package shim

import (
	"fmt"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/marcioapm/lux/internal/proto"
	"github.com/marcioapm/lux/internal/spec"
)

// Services: HTTP services the workload calls through a unix socket each
// (/.lux/services/<name>.sock), with their headers added by the shim. The
// header values come from secrets and live only in this process's memory:
// never in a file, an environment or argv, and the shim is not dumpable
// (see Main), so a workload without CAP_SYS_PTRACE cannot read them from
// /proc either, even as container root.

// ServiceEnv names a service's socket for the workload:
// LUX_SERVICE_<NAME>=unix:/.lux/services/<name>.sock.
func ServiceEnv(name string) (string, string) {
	key := "LUX_SERVICE_" + strings.ToUpper(strings.ReplaceAll(name, "-", "_"))
	return key, "unix:" + ServiceSocket(name)
}

func ServiceSocket(name string) string {
	return filepath.Join(proto.ShimServicesDir, name+".sock")
}

// serveServices starts a proxy per service, its socket owned by the
// workload user (mode 0600).
func (s *Shim) serveServices(secrets map[string]string) error {
	for _, svc := range s.cfg.Services {
		h, err := newServiceProxy(svc, secrets, s.red)
		if err != nil {
			return fmt.Errorf("service %s: %w", svc.Name, err)
		}
		path := ServiceSocket(svc.Name)
		os.Remove(path)
		ln, err := net.Listen("unix", path)
		if err != nil {
			return fmt.Errorf("service %s: %w", svc.Name, err)
		}
		if err := os.Chmod(path, 0o600); err != nil {
			return err
		}
		if err := os.Chown(path, s.user.uid, s.user.gid); err != nil {
			return err
		}
		srv := &http.Server{Handler: h, ReadHeaderTimeout: 30 * time.Second}
		go func() { _ = srv.Serve(ln) }()
	}
	return nil
}

// newServiceProxy forwards every request to svc.URL (its path joined with
// the request's), with svc's headers set over the workload's own.
func newServiceProxy(svc spec.Service, secrets map[string]string, red *Redactor) (http.Handler, error) {
	target, err := url.Parse(svc.URL)
	if err != nil {
		return nil, err
	}
	headers := http.Header{}
	for _, h := range svc.Headers {
		headers.Set(h.Name, secrets[h.Secret])
	}
	transport := &http.Transport{
		// Not the image's HTTP_PROXY: the request goes straight out, under
		// the Run's egress rules.
		Proxy:                 nil,
		DialContext:           (&net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: 0, // streams may take their time
		MaxIdleConns:          16,
		IdleConnTimeout:       90 * time.Second,
		ForceAttemptHTTP2:     true,
	}
	proxy := &httputil.ReverseProxy{
		Rewrite: func(r *httputil.ProxyRequest) {
			r.SetURL(target)
			r.Out.Host = target.Host
			for k, v := range headers {
				r.Out.Header[k] = v
			}
		},
		Transport:     transport,
		FlushInterval: -1, // SSE and chunked responses as they come
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			reason := err.Error()
			if cerr := r.Context().Err(); cerr != nil {
				reason = cerr.Error()
			}
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
			w.WriteHeader(http.StatusBadGateway)
			fmt.Fprintf(w, "lux: service %s unreachable: %s\n", svc.Name, red.Redact(reason))
		},
	}
	// url's path is a prefix the workload can't climb out of: a ".." segment
	// is refused, not cleaned (cleaning would also drop trailing slashes and
	// decode %2F, which APIs care about).
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		for _, seg := range strings.Split(r.URL.Path, "/") {
			if seg == ".." {
				http.Error(w, "lux: a service path may not contain ..", http.StatusBadRequest)
				return
			}
		}
		proxy.ServeHTTP(w, r)
	}), nil
}
