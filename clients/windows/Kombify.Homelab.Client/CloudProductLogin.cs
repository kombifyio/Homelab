#if CLOUD_DESKTOP
using System.Net;
using System.Net.Sockets;
using System.Security.Cryptography;
using System.Text;
using System.Text.Json;
using Kombify.Client.Shell;
using Microsoft.Web.WebView2.Core;

namespace Kombify.Homelab.Client;

internal sealed partial class ClientWindow
{
    private CancellationTokenSource? _cloudProductLoginCancellation;
    private readonly SemaphoreSlim _cloudProductLoginGate = new(1, 1);
    private readonly HttpClient _cloudProductHttpClient = new(new HttpClientHandler
        { AllowAutoRedirect = false, UseCookies = false }) { Timeout = TimeSpan.FromSeconds(15) };
    private static readonly Uri ProductOrigin = new(ClientAuthority.CloudOrigin);
    private const string SessionCookie = "homelab_session";
    private const string OriginCookie = "homelab_session_origin";

    private Task BeginCloudProductLoginAsync() => RunProductLoginAsync(silent: false, CancellationToken.None);

    /// <summary>
    /// One product login through the system browser. Interactive (the person asked) or silent
    /// (device sign-in §4, <c>prompt=none</c>, at most 60 s, never replacing an attempt that
    /// is already running). Returns the account of the session it imported, or null; on any
    /// failure the window shows the ordinary signed-out page, never a sign-in error.
    /// </summary>
    private async Task<DeviceSignInAccount?> RunProductLoginAsync(bool silent, CancellationToken limit,
        string? expectedSubject = null)
    {
        if (silent && _cloudProductLoginCancellation is not null) return null;
        _cloudProductLoginCancellation?.Cancel();
        var attempt = CancellationTokenSource.CreateLinkedTokenSource(limit);
        attempt.CancelAfter(silent ? DeviceSignInFollower.SilentAttemptLimit : TimeSpan.FromMinutes(5));
        _cloudProductLoginCancellation = attempt;
        await _cloudProductLoginGate.WaitAsync();
        try
        {
            if (!ReferenceEquals(_cloudProductLoginCancellation, attempt)) return null;
            return await RunCloudProductLoginAsync(attempt, silent, expectedSubject);
        }
        finally
        {
            _cloudProductLoginGate.Release();
            if (ReferenceEquals(_cloudProductLoginCancellation, attempt))
                _cloudProductLoginCancellation = null;
            attempt.Dispose();
        }
    }

    private async Task<DeviceSignInAccount?> RunCloudProductLoginAsync(CancellationTokenSource attempt, bool silent,
        string? expectedSubject)
    {
        var token = attempt.Token;
        TcpListener? listener = null;
        var importing = false;
        try
        {
            for (var port = 63690; port <= 63694; port++)
            {
                try
                {
                    listener = new TcpListener(IPAddress.Loopback, port);
                    listener.Start(4);
                    break;
                }
                catch (SocketException)
                {
                    listener?.Stop();
                    listener = null;
                }
            }
            if (listener is null)
                throw new InvalidOperationException("No approved loopback login port is available.");

            var verifier = Base64Url(RandomNumberGenerator.GetBytes(32));
            var challenge = Base64Url(SHA256.HashData(Encoding.ASCII.GetBytes(verifier)));
            // The program's own loopback address and attempt state (StackKits #3407): the server's
            // handoff, a refusal included, returns there with exactly this state. No relay takes part.
            var callbackPort = ((IPEndPoint)listener.LocalEndpoint).Port;
            var state = DeviceSignInSilentRoute.State(DeviceSignInSilentRoute.HomelabWindows, callbackPort);
            var startBody = new Dictionary<string, object>
            {
                ["verifier_challenge"] = challenge,
                ["redirect_uri"] = DeviceSignInSilentRoute.LoopbackUri(callbackPort),
                ["state"] = state,
            };
            if (silent) startBody["prompt"] = "none";
            using var startContent = JsonContent(startBody);
            using var start = await _cloudProductHttpClient.PostAsync(new Uri(ProductOrigin, "/api/v2/auth/native/start"), startContent, token);
            if (!start.IsSuccessStatusCode)
                throw new InvalidOperationException("Homelab Runtime could not start this browser login.");
            using var startJSON = JsonDocument.Parse(await start.Content.ReadAsStringAsync(token));
            var loginURL = startJSON.RootElement.GetProperty("login_url").GetString() ?? "";
            if (!Uri.TryCreate(loginURL, UriKind.Absolute, out var loginUri)
                || !ClientAuthority.SameOrigin(loginUri, ProductOrigin)
                || loginUri.AbsolutePath != "/api/v2/auth/login")
                throw new InvalidOperationException("Homelab Runtime returned an invalid browser login URL.");

            if (!silent)
                NavigateHtml(RenderFallback(CloudLoginText.Get("WaitingTitle"),
                    CloudLoginText.Get("WaitingDetail")));
            OpenInBrowser(loginURL);
            var ticket = await AwaitProductCallbackAsync(listener, state, silent, token)
                ?? throw new InvalidOperationException("The silent login was refused.");
            using var redeemContent = JsonContent(new { ticket, verifier, state });
            using var redeem = await _cloudProductHttpClient.PostAsync(new Uri(ProductOrigin, "/api/v2/auth/native/redeem"), redeemContent, token);
            if (!redeem.IsSuccessStatusCode)
                throw new InvalidOperationException("The browser login could not be transferred to this client.");
            using var identityJSON = JsonDocument.Parse(await redeem.Content.ReadAsStringAsync(token));
            var subject = identityJSON.RootElement.GetProperty("subject").GetString() ?? "";
            var tenant = identityJSON.RootElement.GetProperty("tenantId").GetString() ?? "";
            if (subject.Length == 0 || tenant.Length == 0)
                throw new InvalidOperationException("The transferred login has no product identity.");
            var cookies = ParseProductCookies(redeem);
            var email = await VerifyProductIdentityAsync(cookies, subject, tenant, token);
            var account = DeviceSignInAccount.FromIdToken(DeviceSignIn.KombifyIssuer, subject, email, null);
            if (silent && expectedSubject is not null && account.Subject != expectedSubject)
            {
                // Another account than the device's (§4): nothing of it is imported or shown.
                // The product session is a stateless cookie this client never stored, so there is
                // no refresh token of its own to revoke.
                throw new InvalidOperationException("The silent login returned another account.");
            }

            // One WebView profile holds one account's product data. Another account first ends
            // what the previous one left (device sign-in §3: an account change is followed).
            var identityHash = Convert.ToHexString(SHA256.HashData(Encoding.UTF8.GetBytes(subject + "\n" + tenant)));
            var markerPath = ProductIdentityMarkerPath();
            if (File.Exists(markerPath) && File.ReadAllText(markerPath).Trim() != identityHash)
                await ClearLocalProductSessionAsync();
            token.ThrowIfCancellationRequested();
            if (!File.Exists(markerPath))
            {
                Directory.CreateDirectory(Path.GetDirectoryName(markerPath)!);
                using var marker = new FileStream(markerPath, FileMode.CreateNew, FileAccess.Write, FileShare.None);
                marker.Write(Encoding.ASCII.GetBytes(identityHash + "\n"));
                marker.Flush(flushToDisk: true);
            }

            importing = true;
            await ClearProductCookiesAsync();
            token.ThrowIfCancellationRequested();
            ImportProductCookies(cookies);
            await VerifyProductCookiesAsync(cookies);
            token.ThrowIfCancellationRequested();
            SaveDeviceAccount(account);
            _webView.CoreWebView2.Navigate(new Uri(ProductOrigin, "/dashboard").ToString());
            if (!silent && _deviceSignIn is { } follower)
                _ = follower.SignedInInteractivelyAsync(account);
            return account;
        }
        catch (Exception error) when (error is not OutOfMemoryException)
        {
            // Cancelled, refused, timed out or failed: the ordinary signed-out page, no error.
            // A replacement attempt keeps the screen (the gate keeps it from importing until
            // this cleanup ends).
            if (!IsDisposed && importing) await ClearProductCookiesAsync();
            if (!IsDisposed && ReferenceEquals(_cloudProductLoginCancellation, attempt))
            {
                if (await HasProductCookiesAsync()) _webView.CoreWebView2.Navigate(new Uri(ProductOrigin, "/dashboard").ToString());
                else ShowCloudSignedOut();
            }
            return null;
        }
        finally
        {
            listener?.Stop();
        }
    }

    private static string Base64Url(byte[] bytes) => Convert.ToBase64String(bytes)
        .TrimEnd('=').Replace('+', '-').Replace('/', '_');

    /// <summary>
    /// Waits for the browser's return on the loopback port: the ticket, or null when the
    /// product server reports a refused silent attempt (<c>error</c> with this attempt's state).
    /// A silent attempt's landing page shows only the program's name and closes itself where
    /// the browser allows.
    /// </summary>
    private static async Task<string?> AwaitProductCallbackAsync(TcpListener listener, string expectedState,
        bool silent, CancellationToken token)
    {
        while (true)
        {
            using var client = await listener.AcceptTcpClientAsync(token);
            using var stream = client.GetStream();
            using var reader = new StreamReader(stream, Encoding.ASCII, leaveOpen: true);
            // A connection the browser opens and leaves idle (a preconnect) never holds up the
            // real return: its request head must arrive within 10 s, else the next one is taken.
            using var headerLimit = CancellationTokenSource.CreateLinkedTokenSource(token);
            headerLimit.CancelAfter(TimeSpan.FromSeconds(10));
            var requestLine = "";
            var host = "";
            try
            {
                requestLine = await reader.ReadLineAsync(headerLimit.Token) ?? "";
                for (var i = 0; i < 32; i++)
                {
                    var line = await reader.ReadLineAsync(headerLimit.Token);
                    if (line is null || line.Length == 0) break;
                    if (line.StartsWith("Host:", StringComparison.OrdinalIgnoreCase)) host = line[5..].Trim();
                }
            }
            catch (OperationCanceledException) when (!token.IsCancellationRequested)
            {
                continue;
            }
            catch (IOException)
            {
                continue;
            }
            var callback = DeviceSignInSilentRoute.ReadCallback(requestLine, host,
                ((IPEndPoint)listener.LocalEndpoint).Port, expectedState);
            var ticket = callback?.Ticket;
            var refused = callback is { Ticket: null, Error: not null };
            var success = ticket is { Length: 43 };
            string contentType, body;
            if (silent && (success || refused))
            {
                contentType = "text/html; charset=utf-8";
                body = "<!doctype html><meta charset=\"utf-8\"><title>Homelab Runtime</title>"
                    + "<body style=\"font:16px system-ui;margin:48px;color:#1b1d21;background:#f7f9fc\">"
                    + "<p>Homelab Runtime</p><script>setTimeout(() => window.close(), 300)</script></body>";
            }
            else
            {
                contentType = "text/plain; charset=utf-8";
                body = CloudLoginText.Get(success ? "CallbackSuccess" : "CallbackFailure");
            }
            var status = success || refused ? "200 OK" : "400 Bad Request";
            var bytes = Encoding.UTF8.GetBytes($"HTTP/1.1 {status}\r\nContent-Type: {contentType}\r\nCache-Control: no-store\r\nReferrer-Policy: no-referrer\r\nContent-Length: {Encoding.UTF8.GetByteCount(body)}\r\nConnection: close\r\n\r\n{body}");
            await stream.WriteAsync(bytes, token);
            if (success) return ticket!;
            if (refused) return null;
        }
    }

    private sealed record ProductCookie(string Name, string Value, int MaxAge);

    private static IReadOnlyDictionary<string, ProductCookie> ParseProductCookies(HttpResponseMessage response)
    {
        if (!response.Headers.TryGetValues("Set-Cookie", out var values))
            throw new InvalidOperationException("The product session cookies are missing.");
        var cookies = new Dictionary<string, ProductCookie>(StringComparer.Ordinal);
        var headerCount = 0;
        foreach (var line in values)
        {
            if (++headerCount > 16 || line.Length > 8192)
                throw new InvalidOperationException("The product session cookie headers are invalid.");
            var parts = line.Split(';', StringSplitOptions.TrimEntries | StringSplitOptions.RemoveEmptyEntries);
            if (parts.Length == 0) continue;
            var first = parts[0].Split('=', 2);
            if (first.Length != 2 || first[0] is not (SessionCookie or OriginCookie))
                continue;
            if (cookies.ContainsKey(first[0]) || first[1].Length == 0
                || !parts.Any(p => p.Equals("Secure", StringComparison.OrdinalIgnoreCase))
                || !parts.Any(p => p.Equals("HttpOnly", StringComparison.OrdinalIgnoreCase))
                || !parts.Any(p => p.Equals("Path=/", StringComparison.OrdinalIgnoreCase))
                || !parts.Any(p => p.Equals("SameSite=Lax", StringComparison.OrdinalIgnoreCase))
                || parts.Any(p => p.StartsWith("Domain=", StringComparison.OrdinalIgnoreCase)))
                throw new InvalidOperationException("The product session cookie attributes are invalid.");
            var agePart = parts.FirstOrDefault(p => p.StartsWith("Max-Age=", StringComparison.OrdinalIgnoreCase));
            if (agePart is null || !int.TryParse(agePart[8..], out var maxAge) || maxAge <= 0)
                throw new InvalidOperationException("The product session cookie has no valid expiry.");
            cookies.Add(first[0], new ProductCookie(first[0], first[1], maxAge));
        }
        if (cookies.Count != 2) throw new InvalidOperationException("The product session cookies are incomplete.");
        return cookies;
    }

    private async Task ClearProductCookiesAsync()
    {
        if (_webView.CoreWebView2 is null) return;
        foreach (var cookie in await _webView.CoreWebView2.CookieManager.GetCookiesAsync(ProductOrigin.ToString()))
            if (cookie.Name is SessionCookie or OriginCookie)
                _webView.CoreWebView2.CookieManager.DeleteCookie(cookie);
    }

    private void ImportProductCookies(IReadOnlyDictionary<string, ProductCookie> cookies)
    {
        foreach (var entry in cookies.Values)
        {
            var cookie = _webView.CoreWebView2.CookieManager.CreateCookie(entry.Name, entry.Value,
                ProductOrigin.Host, "/");
            cookie.IsSecure = true;
            cookie.IsHttpOnly = true;
            cookie.SameSite = CoreWebView2CookieSameSiteKind.Lax;
            cookie.Expires = DateTime.Now.AddSeconds(entry.MaxAge);
            _webView.CoreWebView2.CookieManager.AddOrUpdateCookie(cookie);
        }
    }

    private async Task VerifyProductCookiesAsync(IReadOnlyDictionary<string, ProductCookie> expected)
    {
        var actual = await _webView.CoreWebView2.CookieManager.GetCookiesAsync(ProductOrigin.ToString());
        foreach (var entry in expected.Values)
            if (!actual.Any(c => c.Name == entry.Name && c.Value == entry.Value))
                throw new InvalidOperationException("The WebView rejected the product session.");
    }

    /// <summary>Checks the imported session against the redeemed identity; returns its email (the device label).</summary>
    private async Task<string?> VerifyProductIdentityAsync(IReadOnlyDictionary<string, ProductCookie> cookies,
        string subject, string tenant, CancellationToken token)
    {
        using var request = new HttpRequestMessage(HttpMethod.Get, new Uri(ProductOrigin, "/api/v2/whoami"));
        request.Headers.TryAddWithoutValidation("Cookie",
            string.Join("; ", cookies.Values.Select(c => c.Name + "=" + c.Value)));
        using var response = await _cloudProductHttpClient.SendAsync(request, token);
        if (!response.IsSuccessStatusCode)
            throw new InvalidOperationException("Homelab Runtime did not accept the transferred session.");
        using var whoami = JsonDocument.Parse(await response.Content.ReadAsStringAsync(token));
        if (whoami.RootElement.GetProperty("subject").GetString() != subject
            || whoami.RootElement.GetProperty("tenantId").GetString() != tenant)
            throw new InvalidOperationException("The transferred session belongs to a different product identity.");
        return whoami.RootElement.TryGetProperty("email", out var email) && email.ValueKind == JsonValueKind.String
            ? email.GetString() : null;
    }

    private async Task<ConnectProductIdentity> CurrentConnectIdentityAsync(CancellationToken token)
    {
        if (IsDisposed || _webView.CoreWebView2 is null)
            throw new ConnectAccountException();
        var actual = await _webView.CoreWebView2.CookieManager.GetCookiesAsync(ProductOrigin.ToString());
        var cookies = new Dictionary<string, string>(StringComparer.Ordinal);
        foreach (var cookie in actual.Where(c => c.Name is SessionCookie or OriginCookie))
        {
            if (cookie.Domain != ProductOrigin.Host || cookie.Path != "/" || !cookie.IsSecure
                || !cookie.IsHttpOnly || cookie.SameSite != CoreWebView2CookieSameSiteKind.Lax
                || cookie.Expires <= DateTime.Now
                || !cookies.TryAdd(cookie.Name, cookie.Value))
                throw new ConnectAccountException();
        }
        if (cookies.Count != 2 || cookies.Values.Any(string.IsNullOrEmpty))
            throw new ConnectAccountException();

        using var request = new HttpRequestMessage(HttpMethod.Get, new Uri(ProductOrigin, "/api/v2/whoami"));
        request.Headers.TryAddWithoutValidation("Cookie", string.Join("; ",
            cookies.Select(c => c.Key + "=" + c.Value)));
        using var response = await _cloudProductHttpClient.SendAsync(request, token);
        if (!response.IsSuccessStatusCode) throw new ConnectAccountException();
        using var document = JsonDocument.Parse(await response.Content.ReadAsStringAsync(token));
        var subject = document.RootElement.GetProperty("subject").GetString();
        var tenant = document.RootElement.GetProperty("tenantId").GetString();
        if (string.IsNullOrWhiteSpace(subject) || string.IsNullOrWhiteSpace(tenant))
            throw new ConnectAccountException();
        var expectedMarker = Convert.ToHexString(SHA256.HashData(
            Encoding.UTF8.GetBytes(subject + "\n" + tenant)));
        var markerPath = ProductIdentityMarkerPath();
        if (!File.Exists(markerPath) || File.ReadAllText(markerPath).Trim() != expectedMarker)
            throw new ConnectAccountException();
        var latest = await _webView.CoreWebView2.CookieManager.GetCookiesAsync(ProductOrigin.ToString());
        var latestProduct = latest.Where(c => c.Name is SessionCookie or OriginCookie).ToArray();
        if (latestProduct.Length != 2 || latestProduct.Any(c => c.Domain != ProductOrigin.Host
            || c.Path != "/" || !c.IsSecure || !c.IsHttpOnly
            || c.SameSite != CoreWebView2CookieSameSiteKind.Lax || c.Expires <= DateTime.Now
            || !cookies.TryGetValue(c.Name, out var value) || c.Value != value))
            throw new ConnectAccountException();
        return new ConnectProductIdentity(subject, tenant);
    }
}
#endif
