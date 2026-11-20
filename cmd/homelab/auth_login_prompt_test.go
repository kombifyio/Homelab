package main

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

const (
	testCallbackPath = "/api/v2/auth/callback"
	testChallenge    = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMN-_1" // 43 characters
)

func authorizeRedirect(extra string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "https://login.kombify.io/authorize?client_id=abc&state=sealed-1"+extra, http.StatusFound)
	})
}

func promptLogin(t *testing.T, inner http.Handler, rawURL string) (url.Values, *httptest.ResponseRecorder) {
	t.Helper()
	recorder := httptest.NewRecorder()
	v2AuthForwardLoginPrompt(inner, testCallbackPath, true).ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, rawURL, nil))
	if recorder.Code != http.StatusFound {
		t.Fatalf("status = %d, want 302", recorder.Code)
	}
	target, err := url.Parse(recorder.Header().Get("Location"))
	if err != nil {
		t.Fatal(err)
	}
	return target.Query(), recorder
}

func TestV2AuthForwardLoginPromptForwardsSilentPrompt(t *testing.T) {
	query, recorder := promptLogin(t, authorizeRedirect(""), "/api/v2/auth/login?prompt=none&return_to=%2Fdashboard")
	if query.Get("prompt") != "none" || query.Has("max_age") || query.Get("client_id") != "abc" || query.Get("state") != "sealed-1" {
		t.Fatalf("authorize query = %v, want prompt=none without max_age and the original parameters", query)
	}
	if len(recorder.Result().Cookies()) != 0 {
		t.Fatalf("a silent browser login needs no attempt cookie, got %v", recorder.Result().Cookies())
	}
}

func TestV2AuthForwardLoginPromptSilentNeverWeakensReauthentication(t *testing.T) {
	inner := authorizeRedirect("&prompt=login&max_age=0")
	query, _ := promptLogin(t, inner, "/api/v2/auth/login?prompt=none&reauth_purpose=wallet.reveal&reauth_resource=w1")
	if query.Get("prompt") != "login" || query.Get("max_age") != "0" {
		t.Fatalf("authorize query = %v, a fresh re-authentication must keep prompt=login&max_age=0", query)
	}
}

func TestV2AuthForwardLoginPromptDropsUnsafePromptValues(t *testing.T) {
	for _, prompt := range []string{"consent", "select_account", "none login", "login none", "NONE", "create", ""} {
		rec := httptest.NewRecorder()
		v2AuthForwardLoginPrompt(authorizeRedirect(""), testCallbackPath, true).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v2/auth/login?prompt="+url.QueryEscape(prompt), nil))
		if location := rec.Header().Get("Location"); location != "https://login.kombify.io/authorize?client_id=abc&state=sealed-1" {
			t.Fatalf("prompt=%q: location = %q, want the untouched authorize URL", prompt, location)
		}
	}
}

func TestV2AuthForwardLoginPromptNamesTheNativeAttemptOfASilentLogin(t *testing.T) {
	returnTo := url.QueryEscape(nativeHandoffPath + "?challenge=" + testChallenge)
	_, recorder := promptLogin(t, authorizeRedirect(""), "/api/v2/auth/login?prompt=none&return_to="+returnTo)
	cookies := recorder.Result().Cookies()
	if len(cookies) != 1 || cookies[0].Name != v2SilentCookieName("sealed-1") || cookies[0].Value != testChallenge ||
		!cookies[0].HttpOnly || !cookies[0].Secure || cookies[0].Path != testCallbackPath {
		t.Fatalf("cookies = %v, want one HttpOnly Secure attempt cookie on the callback path", cookies)
	}

	for _, other := range []string{
		url.QueryEscape("/dashboard"),
		url.QueryEscape(nativeHandoffPath + "?challenge=short"),
		url.QueryEscape("https://evil.example" + nativeHandoffPath + "?challenge=" + testChallenge),
	} {
		_, recorder := promptLogin(t, authorizeRedirect(""), "/api/v2/auth/login?prompt=none&return_to="+other)
		if len(recorder.Result().Cookies()) != 0 {
			t.Fatalf("return_to %q must not name an attempt, got %v", other, recorder.Result().Cookies())
		}
	}
}

func silentCallback(refuse func(*http.Request, string) (string, bool), innerLocation string) http.Handler {
	return v2AuthSilentRefusal(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, innerLocation, http.StatusFound)
	}), testCallbackPath, true, refuse)
}

func TestV2AuthSilentRefusalEndsInTheSignedOutStateWithoutAnError(t *testing.T) {
	for _, code := range []string{"login_required", "consent_required", "interaction_required"} {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, testCallbackPath+"?error="+code+"&state=sealed-1", nil)
		silentCallback(nil, "/login?error="+code).ServeHTTP(rec, req)
		if rec.Code != http.StatusFound || rec.Header().Get("Location") != "/login?manual=1" {
			t.Fatalf("%s: %d %q, want 302 /login?manual=1", code, rec.Code, rec.Header().Get("Location"))
		}
	}
}

func TestV2AuthSilentRefusalKeepsEveryOtherAnswer(t *testing.T) {
	// A state that failed the shared callback's checks, a person's refusal and
	// a login that went wrong keep their error: the wrapper only replaces the
	// exact redirect of a verified silent refusal.
	for _, tc := range []struct{ query, inner string }{
		{"error=login_required&state=forged", "/login?error=invalid_state"},
		{"error=access_denied&state=sealed-1", "/login?error=access_denied"},
		{"error=login_required&state=sealed-1", "/login?error=browser_mismatch"},
		{"code=c&state=sealed-1", "/dashboard"},
	} {
		rec := httptest.NewRecorder()
		silentCallback(nil, tc.inner).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, testCallbackPath+"?"+tc.query, nil))
		if rec.Header().Get("Location") != tc.inner {
			t.Fatalf("%s: location = %q, want %q", tc.query, rec.Header().Get("Location"), tc.inner)
		}
	}
}

func TestV2AuthSilentRefusalReportsARefusedNativeAttemptToItsProgram(t *testing.T) {
	var asked string
	refuse := func(_ *http.Request, challenge string) (string, bool) {
		asked = challenge
		return "http://127.0.0.1:63691/oauth/callback?state=client-state", true
	}
	req := httptest.NewRequest(http.MethodGet, testCallbackPath+"?error=login_required&state=sealed-1", nil)
	req.AddCookie(&http.Cookie{Name: v2SilentCookieName("sealed-1"), Value: testChallenge})
	rec := httptest.NewRecorder()
	silentCallback(refuse, "/login?error=login_required").ServeHTTP(rec, req)

	if asked != testChallenge {
		t.Fatalf("refused attempt = %q, want the cookie's attempt", asked)
	}
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, want 303", rec.Code)
	}
	target, err := url.Parse(rec.Header().Get("Location"))
	if err != nil || target.Host != "127.0.0.1:63691" || target.Path != "/oauth/callback" ||
		target.Query().Get("state") != "client-state" || target.Query().Get("error") != "login_required" ||
		target.Query().Has("ticket") || target.Query().Has("code") {
		t.Fatalf("location = %q, want the program's loopback with its state and the error only", rec.Header().Get("Location"))
	}
	cleared := false
	for _, cookie := range rec.Result().Cookies() {
		if cookie.Name == v2SilentCookieName("sealed-1") && cookie.MaxAge < 0 {
			cleared = true
		}
	}
	if !cleared {
		t.Fatal("the attempt cookie must be cleared")
	}
}

func TestV2AuthSilentRefusalFallsBackWhenTheAttemptIsGone(t *testing.T) {
	refuse := func(*http.Request, string) (string, bool) { return "", false }
	req := httptest.NewRequest(http.MethodGet, testCallbackPath+"?error=consent_required&state=sealed-1", nil)
	req.AddCookie(&http.Cookie{Name: v2SilentCookieName("sealed-1"), Value: testChallenge})
	rec := httptest.NewRecorder()
	silentCallback(refuse, "/login?error=consent_required").ServeHTTP(rec, req)
	if rec.Header().Get("Location") != "/login?manual=1" {
		t.Fatalf("location = %q, want /login?manual=1", rec.Header().Get("Location"))
	}

	// A cookie that does not hold an attempt is never handed to the lookup.
	called := false
	refuse = func(*http.Request, string) (string, bool) { called = true; return "", false }
	req = httptest.NewRequest(http.MethodGet, testCallbackPath+"?error=consent_required&state=sealed-1", nil)
	req.AddCookie(&http.Cookie{Name: v2SilentCookieName("sealed-1"), Value: "x' OR '1'='1"})
	silentCallback(refuse, "/login?error=consent_required").ServeHTTP(httptest.NewRecorder(), req)
	if called {
		t.Fatal("a malformed attempt cookie must be ignored")
	}
}

func TestV2AuthForwardLoginPromptAddsAuthorizePrompt(t *testing.T) {
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "https://login.kombify.io/authorize?client_id=abc&state=1", http.StatusFound)
	})

	req := httptest.NewRequest(http.MethodGet, "/api/v2/auth/login?prompt=login&return_to=%2Fdashboard", nil)
	recorder := httptest.NewRecorder()
	v2AuthForwardLoginPrompt(inner, testCallbackPath, true).ServeHTTP(recorder, req)

	if recorder.Code != http.StatusFound {
		t.Fatalf("status = %d, want 302", recorder.Code)
	}
	location := recorder.Header().Get("Location")
	if !strings.Contains(location, "https://login.kombify.io/authorize") ||
		!strings.Contains(location, "prompt=login") ||
		!strings.Contains(location, "max_age=0") {
		t.Fatalf("location = %q, want Auth0 authorize with prompt=login", location)
	}
}

func TestV2AuthForwardLoginPromptLeavesPlainLoginAlone(t *testing.T) {
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "https://login.kombify.io/authorize?client_id=abc", http.StatusFound)
	})

	req := httptest.NewRequest(http.MethodGet, "/api/v2/auth/login?return_to=%2Fdashboard", nil)
	recorder := httptest.NewRecorder()
	v2AuthForwardLoginPrompt(inner, testCallbackPath, true).ServeHTTP(recorder, req)

	location := recorder.Header().Get("Location")
	if location != "https://login.kombify.io/authorize?client_id=abc" {
		t.Fatalf("location = %q, want untouched authorize URL", location)
	}
}

func TestNativeLoopbackRequestAdmitsOnlyTheProgramsOwnLoopbackAndState(t *testing.T) {
	const nonce = "AbCdEfGhIjKlMnOpQrStUv" // 22 characters, 128 bits
	good := "homelab-windows.63691." + nonce
	if port, ok := nativeLoopbackRequest("http://127.0.0.1:63691/oauth/callback", good); !ok || port != 63691 {
		t.Fatalf("a well-formed request was refused: port=%d ok=%v", port, ok)
	}
	for name, tc := range map[string][2]string{
		"another host":         {"http://localhost:63691/oauth/callback", good},
		"another path":         {"http://127.0.0.1:63691/", good},
		"a query":              {"http://127.0.0.1:63691/oauth/callback?x=1", good},
		"https":                {"https://127.0.0.1:63691/oauth/callback", good},
		"userinfo":             {"http://a@127.0.0.1:63691/oauth/callback", good},
		"a port below":         {"http://127.0.0.1:63689/oauth/callback", "homelab-windows.63689." + nonce},
		"a port above":         {"http://127.0.0.1:63695/oauth/callback", "homelab-windows.63695." + nonce},
		"a state of a port":    {"http://127.0.0.1:63691/oauth/callback", "homelab-windows.63692." + nonce},
		"another client":       {"http://127.0.0.1:63691/oauth/callback", "workbench.63691." + nonce},
		"a short nonce":        {"http://127.0.0.1:63691/oauth/callback", "homelab-windows.63691.short"},
		"a nonce with a dot":   {"http://127.0.0.1:63691/oauth/callback", "homelab-windows.63691." + nonce + ".x"},
		"a nonce with a space": {"http://127.0.0.1:63691/oauth/callback", "homelab-windows.63691.AbCdEfGhIjKlMnOpQrSt U"},
		"a padded port":        {"http://127.0.0.1:63691/oauth/callback", "homelab-windows.063691." + nonce},
		"no state":             {"http://127.0.0.1:63691/oauth/callback", ""},
	} {
		if _, ok := nativeLoopbackRequest(tc[0], tc[1]); ok {
			t.Fatalf("%s was admitted: %q %q", name, tc[0], tc[1])
		}
	}
	if !validNativeState(good) || validNativeState("x") {
		t.Fatal("redeem must accept exactly the attempt state format")
	}
}
