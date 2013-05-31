// Package mux provides a Mux implementation which does
// regexp based URL routing and provides functions for
// managing the lifecycle of a request at different
// points.
package mux

import (
	"fmt"
	"gondola/assets"
	"gondola/cookies"
	"gondola/defaults"
	"gondola/errors"
	"gondola/loaders"
	"gondola/log"
	"gondola/template"
	"net/http"
	"net/http/httputil"
	"reflect"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"
)

type RecoverHandler func(*Context, interface{}) interface{}

// ContextProcessor functions run before the request is matched to
// a handler and might alter the context in any way they see fit
type ContextProcessor func(*Context) bool

type Handler func(*Context)

type Transformer func(Handler) Handler

// ErrorHandler is called before an error is sent
// to the client. The parameters are the current context,
// the error message and the error code. If the handler
// returns true, the error is considered as handled and
// no further data is sent to the client.
type ErrorHandler func(*Context, string, int) bool

type handlerInfo struct {
	host    string
	name    string
	re      *regexp.Regexp
	handler Handler
}

const (
	poolSize = 16
)

type Mux struct {
	ContextProcessors    []ContextProcessor
	ContextFinalizers    []ContextFinalizer
	RecoverHandlers      []RecoverHandler
	handlers             []*handlerInfo
	customContextType    *reflect.Type
	trustXHeaders        bool
	keepRemotePort       bool
	appendSlash          bool
	errorHandler         ErrorHandler
	secret               string
	encryptionKey        string
	defaultCookieOptions *cookies.Options
	assetsManager        assets.Manager
	templatesLoader      loaders.Loader
	templatesMutex       sync.RWMutex
	templatesCache       map[string]Template
	templateProcessors   []TemplateProcessor
	templateVars         map[string]interface{}
	templateVarFuncs     map[string]reflect.Value
	debug                bool

	// Logger to use when logging requests. By default, it's
	// gondola/log/Std, but you can set it to nil to avoid
	// logging at all and gain a bit more of performance.
	Logger       *log.Logger
	contextPool  chan *Context
	maxArguments int
}

// HandleFunc adds an anonymous handler. Anonymous handlers can't be reversed.
func (mux *Mux) HandleFunc(pattern string, handler Handler) {
	mux.HandleHostNamedFunc(pattern, handler, "", "")
}

// HandleNamedFunc adds named handler. Named handlers can be reversed using
// Mux.Reverse() or the "reverse" function in the templates.
func (mux *Mux) HandleNamedFunc(pattern string, handler Handler, name string) {
	mux.HandleHostNamedFunc(pattern, handler, "", name)
}

// HandleHostFunc works like HandleFunc(), but restricts matches to the given host.
func (mux *Mux) HandleHostFunc(pattern string, handler Handler, host string) {
	mux.HandleHostNamedFunc(pattern, handler, host, "")
}

// HandleHostNamedFunc works like HandleNamedFunc(), but restricts matches to the given host.
func (mux *Mux) HandleHostNamedFunc(pattern string, handler Handler, host string, name string) {
	info := &handlerInfo{
		host:    host,
		name:    name,
		re:      regexp.MustCompile(pattern),
		handler: handler,
	}
	mux.handlers = append(mux.handlers, info)
	if m := info.re.NumSubexp() + 1; m > mux.maxArguments {
		mux.maxArguments = m
	}
}

// AddContextProcessor adds context processor to the Mux.
// Context processors run in the same order they were added
// before the mux starts matching the request to a handler and
// may alter the request in any way they see fit as well as
// writing to the context. If any of the processors returns
// true, the request is considered as served and no further
// processing to it is done.
func (mux *Mux) AddContextProcessor(cp ContextProcessor) {
	mux.ContextProcessors = append(mux.ContextProcessors, cp)
}

// AddContextFinalizer adds a context finalizer to the mux.
// Context finalizers run in the order they were added and
// after the request has been served (even if it was stopped by
// a context processor). They're intended as a way to perform
// some logging or cleanup (e.g. closing database connections
// that might have been opened during the request lifetime)
func (mux *Mux) AddContextFinalizer(cf ContextFinalizer) {
	mux.ContextFinalizers = append(mux.ContextFinalizers, cf)
}

// AddRecoverHandler adds a recover handler to the mux.
// Recover handlers are executed in the order they were added in case
// there's a panic while serving a request. The handlers may write
// to the context. If any recover handler returns nil the error is
// considered as handled and no panic is raised.
func (mux *Mux) AddRecoverHandler(rh RecoverHandler) {
	mux.RecoverHandlers = append(mux.RecoverHandlers, rh)
}

// SetCustomContextType sets the context type returned by mux/Context.Custom()
// which must be convertible to mux.Context.
// See the documentation on mux/Context.Custom() for further information.
func (mux *Mux) SetCustomContextType(ctx interface{}) {
	t := reflect.TypeOf(ctx)
	if t.Kind() == reflect.Struct {
		t = reflect.PtrTo(t)
	}
	contextType := reflect.TypeOf(&Context{})
	if !t.ConvertibleTo(contextType) {
		panic(fmt.Errorf("Custom context type must convertible to %v", contextType))
	}
	/* All checks passed */
	mux.customContextType = &t
}

// TrustsXHeaders returns if the mux uses X headers
// for determining the remote IP and scheme. See SetTrustXHeaders()
// for a more detailed explanation.
func (mux *Mux) TrustsXHeaders() bool {
	return mux.trustXHeaders
}

// SetTrustXHeaders sets if the mux uses X headers like
// X-Real-IP, X-Forwarded-For, X-Scheme and X-Forwarded-Proto
// to override the remote IP and scheme. This is useful
// when running your application behind a proxy or load balancer.
// The default is disabled. Please, keep in mind that enabling
// XHeaders processing when not running behind a proxy or load
// balancer which sanitizes the input *IS A SECURITY RISK*.
func (mux *Mux) SetTrustXHeaders(t bool) {
	mux.trustXHeaders = t
}

// KeepsRemotePort returns if the mux keeps the remote port
// in http.Request.RemoteAddr field. See SetKeepRemotePort
// for a more detailed explanation.
func (mux *Mux) KeepsRemotePort() bool {
	return mux.keepRemotePort
}

// SetKeepRemovePort sets if the mux keeps the remote port
// in http.Request.RemoteAddr field. Since the remote port
// is rarely useful, this defaults to false, so RemoteAddr
// will only contain an address
func (mux *Mux) SetKeepRemotePort(k bool) {
	mux.keepRemotePort = k
}

// AppendSlash returns if the mux will automatically append
// a slash when appropriate. See SetAppendSlash for a more
// detailed description.
func (mux *Mux) AppendsSlash() bool {
	return mux.appendSlash
}

// SetAppendSlash enables or disables automatic slash appending.
// When enabled, GET and HEAD requests for /foo will be
// redirected to /foo/ if there's a valid handler for that URL,
// rather than returning a 404. The default is true.
func (mux *Mux) SetAppendSlash(b bool) {
	mux.appendSlash = b
}

// Secret returns the secret for this mux. See
// SetSecret() for further details.
func (mux *Mux) Secret() string {
	return mux.secret
}

// SetSecret sets the secret associated with this mux,
// which is used for signed cookies. It should be a
// random string with at least 32 characters. When the
// mux is initialized, this value is set to the value
// returned by defaults.Secret() (which can be controlled
// from the config).
func (mux *Mux) SetSecret(secret string) {
	mux.secret = secret
}

// EncryptionKey returns the encryption key for this
// mux. See SetEncryptionKey() for details.
func (mux *Mux) EncryptionKey() string {
	return mux.encryptionKey
}

// SetEncriptionKey sets the encryption key for this
// mux, which is used by encrypted cookies. It should
// be a random string of 16, 24 or 32 characters.
func (mux *Mux) SetEncryptionKey(key string) {
	mux.encryptionKey = key
}

// DefaultCookieOptions returns the default options
// used for cookies. This is initialized to the value
// returned by cookies.Defaults(). See gondola/cookies
// documentation for more details.
func (mux *Mux) DefaultCookieOptions() *cookies.Options {
	return mux.defaultCookieOptions
}

// SetDefaultCookieOptions sets the default cookie options
// for this mux. See gondola/cookies documentation for more
// details.
func (mux *Mux) SetDefaultCookieOptions(o *cookies.Options) {
	mux.defaultCookieOptions = o
}

// ErrorHandler returns the error handler (if any)
// associated with this mux
func (mux *Mux) ErrorHandler() ErrorHandler {
	return mux.errorHandler
}

// SetErrorHandler sets the error handler for this mux.
// See the documentation on ErrorHandler for a more
// detailed description.
func (mux *Mux) SetErrorHandler(handler ErrorHandler) {
	mux.errorHandler = handler
}

// AssetsManager returns the manager for static assets
func (mux *Mux) AssetsManager() assets.Manager {
	return mux.assetsManager
}

// SetAssetsManager sets the static assets manager for the mux. See
// the documention on gondola/assets/Manager for further information.
func (mux *Mux) SetAssetsManager(manager assets.Manager) {
	manager.SetDebug(mux.Debug())
	mux.assetsManager = manager
}

// TemplatesLoader returns the loader for the templates assocciated
// with this mux. By default, templates will be loaded from the
// tmpl directory relative to the application binary.
func (mux *Mux) TemplatesLoader() loaders.Loader {
	return mux.templatesLoader
}

// SetTemplatesLoader sets the loader used to load the templates
// associated with this mux. By default, templates will be loaded from the
// tmpl directory relative to the application binary.
func (mux *Mux) SetTemplatesLoader(loader loaders.Loader) {
	mux.templatesLoader = loader
}

// AddTemplateProcessor adds a new template processor. Template processors
// may modify a template after it's been loaded.
func (mux *Mux) AddTemplateProcessor(processor TemplateProcessor) {
	mux.templateProcessors = append(mux.templateProcessors, processor)
}

// AddTemplateVars adds additional variables which will be passed
// to the templates executed by this mux. The values in the map might
// either be values or functions which receive a *Context instance and return
// either one or two values (the second one must be an error), in which case
// they will be called with the current context to obtain the variable
// that will be passed to the template. You must call this
// function before any templates have been compiled. The value for
// each variable in the map is its default value, and it can
// be overriden by using ExecuteVars() rather than Execute() when
// executing the template.
func (mux *Mux) AddTemplateVars(vars template.VarMap) {
	if mux.templateVars == nil {
		mux.templateVars = make(template.VarMap)
		mux.templateVarFuncs = make(map[string]reflect.Value)
	}
	for k, v := range vars {
		if mux.isReservedVariable(k) {
			panic(fmt.Errorf("Variable %s is reserved", k))
		}
		if t := reflect.TypeOf(v); t.Kind() == reflect.Func {
			inType := reflect.TypeOf(&Context{})
			if t.NumIn() != 1 || t.In(0) != inType {
				panic(fmt.Errorf("Template variable functions must receive a single %s argument", inType))
			}
			if t.NumOut() > 2 {
				panic(fmt.Errorf("Template variable functions must return at most 2 arguments"))
			}
			if t.NumOut() == 2 {
				o := t.Out(1)
				if o.Kind() != reflect.Interface || o.Name() != "error" {
					panic(fmt.Errorf("Template variable functions must return an error as their second argument"))
				}
			}
			mux.templateVarFuncs[k] = reflect.ValueOf(v)
		} else {
			mux.templateVars[k] = v
		}
	}
}

// LoadTemplate loads a template using the template
// loader and the asset manager assocciated with
// this mux
func (mux *Mux) LoadTemplate(name string) (Template, error) {
	mux.templatesMutex.RLock()
	tmpl := mux.templatesCache[name]
	mux.templatesMutex.RUnlock()
	if tmpl == nil {
		t := newTemplate(mux)
		vars := make(template.VarMap, len(mux.templateVars)+len(mux.templateVarFuncs))
		for k, v := range mux.templateVars {
			vars[k] = v
		}
		for k, _ := range mux.templateVarFuncs {
			vars[k] = nil
		}
		err := t.ParseVars(name, vars)
		if err != nil {
			return nil, err
		}
		for _, v := range mux.templateProcessors {
			t.Template, err = v(t.Template)
			if err != nil {
				return nil, err
			}
		}
		tmpl = t
		if !mux.debug {
			mux.templatesMutex.Lock()
			mux.templatesCache[name] = tmpl
			mux.templatesMutex.Unlock()
		}
	}
	return tmpl, nil
}

// Debug returns if the mux is in debug mode
// (i.e. templates are not cached).
func (mux *Mux) Debug() bool {
	return mux.debug
}

// SetDebug sets the debug state for the mux.
// When true, templates executed via Context.Execute or
// Context.MustExecute() are recompiled every time
// they are executed. The default is the value
// returned by defaults.Debug() when the mux is
// constructed. See the documentation on gondola/defaults
// for further information.
func (mux *Mux) SetDebug(debug bool) {
	mux.debug = debug
}

// HandleAssets adds several handlers to the mux which handle
// assets efficiently and allows the use of the "assset"
// function from the templates. This function will also modify the
// asset loader associated with this mux. prefix might be a relative
// (e.g. /static/) or absolute (e.g. http://static.example.com/) url
// while dir should be the path to the directory where the static
// assets reside. You probably want to use RelativePath() in gondola/util
// to define the directory relative to the application binary. Note
// that /favicon.ico and /robots.txt will be handled too, but they
// will must be in the directory which contains the rest of the assets.
func (mux *Mux) HandleAssets(prefix string, dir string) {
	loader := loaders.NewFSLoader(dir)
	mux.SetAssetsManager(assets.NewAssetsManager(loader, prefix))
	assetsHandler := assets.Handler(mux.assetsManager)
	handler := func(ctx *Context) {
		assetsHandler(ctx, ctx.R)
	}
	mux.HandleFunc("^"+prefix, handler)
	mux.HandleFunc("^/favicon.ico$", handler)
	mux.HandleFunc("^/robots.txt$", handler)
}

// MustReverse calls Reverse and panics if it finds an error. See
// Reverse for further details.
func (mux *Mux) MustReverse(name string, args ...interface{}) string {
	rev, err := mux.Reverse(name, args...)
	if err != nil {
		panic(err)
	}
	return rev
}

// Reverse obtains the url given a handler name and its arguments.
// The number of arguments must be equal to the
// number of captured parameters in the patttern for the handler
// e.g. given the pattern ^/article/\d+/[\w\-]+/$, you should provide
// 2 arguments and passing 42 and "the-ultimate-answer-to-life-the-universe-and-everything"
// would return "/article/42/the-ultimate-answer-to-life-the-universe-and-everything/"
// If the handler is also restricted to a given hostname, the return value
// will be a scheme relative url e.g. //www.example.com/article/...
func (mux *Mux) Reverse(name string, args ...interface{}) (string, error) {
	if name == "" {
		return "", fmt.Errorf("No handler name specified")
	}
	for _, v := range mux.handlers {
		if v.name == name {
			reversed, err := FormatRegexp(v.re, true, args...)
			if err != nil {
				if acerr, ok := err.(*ArgumentCountError); ok {
					if acerr.MinArguments == acerr.MaxArguments {
						return "", fmt.Errorf("Handler %q requires exactly %d arguments, %d received instead",
							name, acerr.MinArguments, len(args))
					}
					return "", fmt.Errorf("Handler %q requires at least %d arguments and at most %d arguments, %d received instead",
						name, acerr.MinArguments, acerr.MaxArguments, len(args))
				}
				return "", fmt.Errorf("Error reversing handler %q: %s", name, err)
			}
			if v.host != "" {
				reversed = fmt.Sprintf("//%s%s", v.host, reversed)
			}
			return reversed, nil
		}
	}
	return "", fmt.Errorf("No handler named %q", name)
}

// ListenAndServe Starts listening on all the interfaces on the given port.
// If port is <= 0, defaults.Port() will be used instead
// If you need more granularity you can use http.ListenAndServe() directly
// e.g.
// http.ListenAndServe("127.0.0.1:8000", mymux)
func (mux *Mux) ListenAndServe(port int) error {
	if port <= 0 {
		port = defaults.Port()
	}
	log.Infof("Listening on port %d", port)
	return http.ListenAndServe(":"+strconv.Itoa(port), mux)
}

// MustListenAndServe works like ListenAndServe, but panics if
// there's an error
func (mux *Mux) MustListenAndServe(port int) {
	err := mux.ListenAndServe(port)
	if err != nil {
		log.Panicf("Error listening on port %d: %s", port, err)
	}
}

func (mux *Mux) readXHeaders(r *http.Request) {
	realIp := r.Header.Get("X-Real-IP")
	if realIp != "" {
		r.RemoteAddr = realIp
	} else {
		forwardedFor := r.Header.Get("X-Forwarded-For")
		if forwardedFor != "" {
			r.RemoteAddr = forwardedFor
		}
	}
	/*
		Don't set the scheme, because net/http does not set the
		Host portion of the URL, this by setting the Scheme but
		not the Host .String() ends up returning and invalid URL.
		This needs further investigation until a proper solution
		can be found (ignore scheme headers or also set the Host
		property in the URL?)
		xScheme := r.Header.Get("X-Scheme")
		if xScheme != "" {
			r.URL.Scheme = xScheme
		} else {
			xForwardedProto := r.Header.Get("X-Forwarded-Proto")
			if xForwardedProto != "" {
				r.URL.Scheme = xForwardedProto
			}
		}
	*/
}

func (mux *Mux) handleHTTPError(ctx *Context, error string, code int) {
	if mux.errorHandler == nil || !mux.errorHandler(ctx, error, code) {
		http.Error(ctx, error, code)
	}
}

func (mux *Mux) handleError(ctx *Context, err interface{}) bool {
	if gerr, ok := err.(errors.Error); ok {
		mux.handleHTTPError(ctx, gerr.Error(), gerr.StatusCode())
		return true
	}
	return false
}

func (mux *Mux) recover(ctx *Context) {
	if err := recover(); err != nil {
		for _, v := range mux.RecoverHandlers {
			err = v(ctx, err)
			if err == nil {
				break
			}
		}
		if err != nil && !mux.handleError(ctx, err) {
			stack := ""
			const size = 4096
			buf := make([]byte, size)
			buf = buf[:runtime.Stack(buf, false)]
			// Remove lines 1 and 2, since they correspond
			// to this very function and don't add any
			// useful information
			lines := strings.Split(string(buf), "\n")
			if len(lines) > 2 {
				lines = append(lines[:1], lines[3:]...)
				stack = strings.Join(lines, "\n")
			}
			req := ""
			dump, derr := httputil.DumpRequest(ctx.R, true)
			if derr == nil {
				req = string(dump)
			}
			log.Errorf("Panic serving %v %v %v: %v\n\nStack:\n%s\nRequest:\n%s", ctx.R.Method, ctx.R.URL, ctx.R.RemoteAddr, err, stack, req)
			mux.handleHTTPError(ctx, "Internal Server Error", http.StatusInternalServerError)
		}
	}
}

// ServeHTTP is called from the net/http system. You shouldn't need
// to call this function
func (mux *Mux) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	ctx := mux.NewContext(nil)
	ctx.ResponseWriter = w
	ctx.R = r
	defer mux.CloseContext(ctx)
	defer mux.recover(ctx)
	if mux.trustXHeaders {
		mux.readXHeaders(r)
	}
	if !mux.keepRemotePort {
		addr := r.RemoteAddr
		if strings.Count(addr, ".") == 3 {
			/* IPv4 e.g. 127.0.0.1:8000 */
			idx := strings.Index(addr, ":")
			if idx >= 0 {
				r.RemoteAddr = addr[:idx]
			}
		} else {
			/* IPv6 e.g. [1fff:0:a88:85a3::ac1f]:8001 */
			if addr != "" && addr[0] == '[' {
				idx := strings.Index(addr, "]")
				if idx >= 0 {
					r.RemoteAddr = addr[1:idx]
				}
			}
		}
	}
	for _, v := range mux.ContextProcessors {
		if v(ctx) {
			return
		}
	}

	if h := mux.matchHandler(r, ctx); h != nil {
		h.handler(ctx)
		return
	}

	mux.appendSlash = true
	if mux.appendSlash && (r.Method == "GET" || r.Method == "HEAD") && !strings.HasSuffix(r.URL.Path, "/") {
		r.URL.Path += "/"
		match := mux.matchHandler(r, ctx)
		if match != nil {
			ctx.Redirect(r.URL.String(), true)
			r.URL.Path = r.URL.Path[:len(r.URL.Path)-1]
			return
		}
		r.URL.Path = r.URL.Path[:len(r.URL.Path)-1]
	}

	/* Not found */
	mux.handleHTTPError(ctx, "Not Found", http.StatusNotFound)
}

func (mux *Mux) matchHandler(r *http.Request, ctx *Context) *handlerInfo {
	p := r.URL.Path
	for _, v := range mux.handlers {
		if v.host != "" && v.host != r.Host {
			continue
		}
		// Use FindStringSubmatchIndex, since this way we can
		// reuse the slices used to store context arguments
		if m := v.re.FindStringSubmatchIndex(p); m != nil {
			n := v.re.NumSubexp() + 1
			for ii := 0; ii < n; ii++ {
				if x := 2 * ii; x < len(m) && m[x] >= 0 {
					ctx.arguments = append(ctx.arguments, p[m[x]:m[x+1]])
				}
			}
			ctx.re = v.re
			ctx.handlerName = v.name
			return v
		}
	}
	return nil
}

// NewContext initializes and returns a new context
// asssocciated with this mux.
func (mux *Mux) NewContext(args []string) *Context {
	var ctx *Context
	select {
	case ctx = <-mux.contextPool:
		ctx.reset(mux.maxArguments)
	default:
		arguments := make([]string, 0, mux.maxArguments)
		ctx = &Context{arguments: arguments, mux: mux, started: time.Now()}
	}
	ctx.arguments = append(ctx.arguments, args...)
	return ctx
}

// CloseContext closes the passed context, which should have been
// created via NewContext(). Keep in mind that this function is
// called for you most of the time. As a rule of thumb, if you
// don't call NewContext() yourself, you don't need to call
// CloseContext().
func (mux *Mux) CloseContext(ctx *Context) {
	for _, v := range mux.ContextFinalizers {
		v(ctx)
	}
	ctx.Close()
	level := log.LInfo
	switch {
	case ctx.statusCode >= 400 && ctx.statusCode < 500:
		level = log.LWarning
	case ctx.statusCode >= 500:
		level = log.LError
	}
	if mux.Logger != nil && ctx.R != nil {
		mux.Logger.Log(level, strings.Join([]string{ctx.R.Method, ctx.R.RequestURI, ctx.R.RemoteAddr,
			strconv.Itoa(ctx.statusCode), time.Now().Sub(ctx.started).String()}, " "))
	}
	select {
	case mux.contextPool <- ctx:
	default:
	}

}

func (mux *Mux) isReservedVariable(va string) bool {
	for _, v := range reservedVariables {
		if v == va {
			return true
		}
	}
	return false
}

// Returns a new Mux initialized with the current default values.
// See gondola/defaults for further information.
func New() *Mux {
	return &Mux{
		debug:          defaults.Debug(),
		secret:         defaults.Secret(),
		encryptionKey:  defaults.EncryptionKey(),
		appendSlash:    true,
		templatesCache: make(map[string]Template),
		Logger:         log.Std,
		contextPool:    make(chan *Context, poolSize),
	}
}
