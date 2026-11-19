package main

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Techstack's login URL passes exactly two OIDC `prompt` values on to the
// upstream authorize redirect; every other value is dropped.
//
//   - login: the wallet-reveal fresh re-authentication (prompt=login&max_age=0).
//   - none: the silent sign-in of a desktop program (device sign-in contract
//     v1, section 4). Session renewal never sends it.
const (
	v2PromptLogin = "login"
	v2PromptNone  = "none"

	v2SilentCookiePrefix = "techstack_silent_"

	nativeHandoffPath      = "/api/v2/auth/native/handoff"
	nativeChallengeLength  = 43
	v2SilentLoginErrorPath = "/login?manual=1"
)

// The answers of Auth0 to a prompt=none request that cannot be completed
// without a form. They carry no session and no fault of the person.
var v2SilentRefusals = map[string]bool{
	"login_required":       true,
	"consent_required":     true,
	"interaction_required": true,
}

// v2AuthForwardLoginPrompt forwards ?prompt=login and ?prompt=none from
// Techstack's login URL onto the upstream Auth0 authorize redirect.
//
// A fresh re-authentication (reauth_purpose or reauth_resource) is never
// weakened to a silent request: prompt=none is ignored there and the shared
// flow's own prompt=login stays.
//
// A silent request that starts a native login (return_to is the native
// handoff of one attempt) also leaves a short-lived cookie bound to its state
// that names the attempt, so a refusal can be reported to the program that
// asked (v2AuthSilentRefusal).
func v2AuthForwardLoginPrompt(next http.Handler, callbackPath string, secure bool) http.Handler {
	if next == nil {
		return nil
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r == nil {
			next.ServeHTTP(w, r)
			return
		}
		query := r.URL.Query()
		prompt := strings.TrimSpace(query.Get("prompt"))
		silent := prompt == v2PromptNone &&
			strings.TrimSpace(query.Get("reauth_purpose")) == "" &&
			strings.TrimSpace(query.Get("reauth_resource")) == ""
		if prompt != v2PromptLogin && !silent {
			next.ServeHTTP(w, r)
			return
		}

		recorder := httptest.NewRecorder()
		next.ServeHTTP(recorder, r.Clone(r.Context()))
		location := strings.TrimSpace(recorder.Header().Get("Location"))
		if recorder.Code < http.StatusMultipleChoices ||
			recorder.Code >= http.StatusBadRequest ||
			location == "" {
			copyRecordedHTTPResponse(w, recorder)
			return
		}

		target, err := url.Parse(location)
		if err != nil || !target.IsAbs() || target.Host == "" {
			copyRecordedHTTPResponse(w, recorder)
			return
		}

		authorize := target.Query()
		if silent {
			authorize.Set("prompt", v2PromptNone)
			authorize.Del("max_age")
			if challenge := nativeChallengeOf(query.Get("return_to")); challenge != "" {
				if state := strings.TrimSpace(authorize.Get("state")); state != "" {
					http.SetCookie(w, v2SilentCookie(state, challenge, callbackPath, secure, int(v2LoginStateCookieMaxAge/time.Second)))
				}
			}
		} else {
			authorize.Set("prompt", v2PromptLogin)
			authorize.Set("max_age", "0")
		}
		target.RawQuery = authorize.Encode()

		for key, values := range recorder.Header() {
			if strings.EqualFold(key, "Location") {
				continue
			}
			for _, value := range values {
				w.Header().Add(key, value)
			}
		}
		http.Redirect(w, r, target.String(), recorder.Code)
	})
}

// v2AuthSilentRefusal ends a refused silent sign-in in the ordinary signed-out
// state. Auth0 answers prompt=none with login_required, consent_required or
// interaction_required when it would have to show a form; the shared callback
// turns every error into /login?error=<code>, an error message for a person
// who asked for nothing. Only that exact redirect, produced after the shared
// callback verified the sealed state and its browser binding, is replaced:
//
//   - a silent attempt of a desktop program is reported to the loopback port
//     that program listens on (error and its own state, no ticket), so it stops
//     waiting;
//   - any other browser lands on /login?manual=1 (the signed-out sign-in
//     page, no error and no automatic second attempt).
//
// An invalid or foreign state keeps its error.
func v2AuthSilentRefusal(next http.Handler, callbackPath string, secure bool, refuse func(r *http.Request, challenge string) (string, bool)) http.Handler {
	if next == nil {
		return nil
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r == nil {
			next.ServeHTTP(w, r)
			return
		}
		query := r.URL.Query()
		code := strings.TrimSpace(query.Get("error"))
		if !v2SilentRefusals[code] {
			next.ServeHTTP(w, r)
			return
		}

		recorder := httptest.NewRecorder()
		next.ServeHTTP(recorder, r.Clone(r.Context()))
		if recorder.Code < http.StatusMultipleChoices || recorder.Code >= http.StatusBadRequest ||
			strings.TrimSpace(recorder.Header().Get("Location")) != "/login?error="+url.QueryEscape(code) {
			copyRecordedHTTPResponse(w, recorder)
			return
		}

		for key, values := range recorder.Header() {
			if strings.EqualFold(key, "Location") {
				continue
			}
			for _, value := range values {
				w.Header().Add(key, value)
			}
		}
		state := strings.TrimSpace(query.Get("state"))
		location := v2SilentLoginErrorPath
		var attempt string
		if cookie, err := r.Cookie(v2SilentCookieName(state)); err == nil && validNativeChallenge(cookie.Value) {
			attempt = cookie.Value
		}
		if attempt != "" {
			http.SetCookie(w, v2SilentCookie(state, "", callbackPath, secure, -1))
			if refuse != nil {
				if loopback, ok := refuse(r, attempt); ok {
					if target, err := url.Parse(loopback); err == nil {
						q := target.Query()
						q.Set("error", code)
						target.RawQuery = q.Encode()
						w.Header().Set("Cache-Control", "no-store")
						w.Header().Set("Referrer-Policy", "no-referrer")
						w.Header().Set("Content-Security-Policy", "default-src 'none'")
						http.Redirect(w, r, target.String(), http.StatusSeeOther)
						return
					}
				}
			}
		}
		http.Redirect(w, r, location, http.StatusFound)
	})
}

func v2SilentCookieName(state string) string {
	return v2SilentCookiePrefix + v2LoginStateDigest(state)[:16]
}

func v2SilentCookie(state, challenge, callbackPath string, secure bool, maxAge int) *http.Cookie {
	value := ""
	if maxAge > 0 {
		value = challenge
	}
	return &http.Cookie{
		Name:     v2SilentCookieName(state),
		Value:    value,
		Path:     callbackPath,
		MaxAge:   maxAge,
		HttpOnly: true,
		Secure:   secure,
		SameSite: http.SameSiteLaxMode,
	}
}

// nativeChallengeOf returns the attempt a login's return_to hands the browser
// to, or "" when return_to is anything else.
func nativeChallengeOf(returnTo string) string {
	target, err := url.Parse(strings.TrimSpace(returnTo))
	if err != nil || target.IsAbs() || target.Host != "" || target.Path != nativeHandoffPath {
		return ""
	}
	challenge := target.Query().Get("challenge")
	if !validNativeChallenge(challenge) {
		return ""
	}
	return challenge
}

// The program that starts a native login names its own loopback address and
// attempt: redirect_uri is exactly http://127.0.0.1:<port>/oauth/callback on
// one of the approved ports, and state is <client>.<port>.<nonce> with the
// same port and a nonce of at least 128 bits (22 base64url characters). The
// browser's last hop, a refusal of a silent sign-in included, returns to that
// address with that state.
const (
	nativeClientName   = "techstack-windows"
	nativePortFirst    = 63690
	nativePortLast     = 63694
	nativeNonceMinimum = 22
	nativeNonceMaximum = 64
)

// nativeLoopbackRequest returns the loopback port of a valid redirect_uri and
// state pair.
func nativeLoopbackRequest(redirectURI, state string) (int, bool) {
	target, err := url.Parse(redirectURI)
	if err != nil || target.Scheme != "http" || target.Hostname() != "127.0.0.1" {
		return 0, false
	}
	port, err := strconv.Atoi(target.Port())
	if err != nil || port < nativePortFirst || port > nativePortLast ||
		redirectURI != "http://127.0.0.1:"+strconv.Itoa(port)+nativeCallbackPath {
		return 0, false
	}
	if statePort, ok := nativeStatePort(state); !ok || statePort != port {
		return 0, false
	}
	return port, true
}

// validNativeState reports whether value is a well-formed attempt state.
func validNativeState(value string) bool {
	_, ok := nativeStatePort(value)
	return ok
}

func nativeStatePort(state string) (int, bool) {
	parts := strings.Split(state, ".")
	if len(parts) != 3 || parts[0] != nativeClientName {
		return 0, false
	}
	port, err := strconv.Atoi(parts[1])
	if err != nil || port < nativePortFirst || port > nativePortLast || parts[1] != strconv.Itoa(port) {
		return 0, false
	}
	nonce := parts[2]
	if len(nonce) < nativeNonceMinimum || len(nonce) > nativeNonceMaximum {
		return 0, false
	}
	for _, c := range nonce {
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == '-', c == '_':
		default:
			return 0, false
		}
	}
	return port, true
}

func validNativeChallenge(value string) bool {
	if len(value) != nativeChallengeLength {
		return false
	}
	for _, c := range value {
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == '-', c == '_':
		default:
			return false
		}
	}
	return true
}

func copyRecordedHTTPResponse(w http.ResponseWriter, recorder *httptest.ResponseRecorder) {
	for key, values := range recorder.Header() {
		for _, value := range values {
			w.Header().Add(key, value)
		}
	}
	w.WriteHeader(recorder.Code)
	_, _ = w.Write(recorder.Body.Bytes())
}
