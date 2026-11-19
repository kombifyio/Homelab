#if CLOUD_DESKTOP
using System.Text.Json;
using Kombify.Client.Shell;
using Microsoft.Web.WebView2.Core;

namespace Kombify.TechStack.Client;

// One sign-in per device (workspace platform/standards/desktop-device-sign-in.v1.md): the
// Connected edition follows the shared state of the kombify desktop programs and writes it
// after its own sign-in and a person's sign-out. Its own session stays the product cookie in
// its own WebView2 profile; nothing of it is shared. No screen here ever says that a sign-in
// failed or that another program signed the person out: it shows its ordinary pages.
internal sealed partial class ClientWindow
{
    private DeviceSignInFollower? _deviceSignIn;
    private bool _personSigningOut;

    private bool IsCloudProductMode() =>
        !DesktopEdition.IsLocal && !IsLocalMode() && !IsServerMode() && _boundServerProfile is null;

    private static string ProductIdentityMarkerPath() =>
        Path.Combine(ClientConfig.StateDirectory(), "webview2-cloud-product-v1", "identity");

    // The account of this client's own session as the shared state names it (subject hash and
    // label); not a credential. Deleted with the session.
    private static string DeviceAccountPath() =>
        Path.Combine(ClientConfig.StateDirectory(), "device-sign-in-account.json");

    /// <summary>
    /// The launch read (§3): returns the first page. With a present state and no matching
    /// session of its own, the window opens on the ordinary signed-out page, so that the login
    /// page does not start an interactive browser login while the follower decides.
    /// </summary>
    private async Task<string> StartDeviceSignInAsync(string launchUrl)
    {
        if (!IsCloudProductMode() || !DeviceSignInFollower.Enabled) return launchUrl;
        var session = new TechstackDeviceSession(this);
        _deviceSignIn = new DeviceSignInFollower(session);
        Activated += (_, _) => _deviceSignIn?.OnActivated();
        FormClosed += (_, _) => _deviceSignIn?.Dispose();
        try
        {
            var state = await Task.Run(DeviceSignIn.Read);
            if (state.Kind is DeviceSignInKind.Absent or DeviceSignInKind.NotReadable) return launchUrl;
            var own = await session.CurrentAccountAsync(CancellationToken.None);
            return state.Kind == DeviceSignInKind.SignedIn && own?.Subject == state.Subject
                ? launchUrl
                : SignedOutUrl(personSignedOut: false);
        }
        catch (Exception error) when (error is not OutOfMemoryException)
        {
            return launchUrl;
        }
    }

    private static string SignedOutUrl(bool personSignedOut) =>
        new Uri(ProductOrigin, personSignedOut ? "/login?manual=1&logged_out=1" : "/login?manual=1").ToString();

    /// <summary>The ordinary signed-out page: the product's sign-in page without an automatic browser login.</summary>
    private void ShowCloudSignedOut(bool personSignedOut = false)
    {
        if (IsDisposed || _webView.CoreWebView2 is null) return;
        _webView.CoreWebView2.Navigate(SignedOutUrl(personSignedOut));
    }

    /// <summary>A navigation that is the product's own sign-out, which only the person starts (account menu, settings).</summary>
    private static bool IsProductSignOutNavigation(Uri uri) =>
        ClientAuthority.SameOrigin(uri, ProductOrigin)
        && uri.AbsolutePath is "/api/v2/auth/logout" or "/auth/logout";

    /// <summary>
    /// A person signed out here (§5): (1) signed_out first, (2) this client's own session,
    /// (3) the browser's Universal Login session through the product's own sign-out in the
    /// system browser, which ends at Auth0 with Techstack's client id and lands on the product's
    /// signed-out page. A failure of (2) or (3) never undoes (1).
    /// </summary>
    private async Task PersonSignOutAsync()
    {
        if (_personSigningOut) return;
        _personSigningOut = true;
        try
        {
            if (_deviceSignIn is { } follower) await follower.SignedOutByPersonAsync();
            _cloudProductLoginCancellation?.Cancel();
            await ClearLocalProductSessionAsync();
            ShowCloudSignedOut(personSignedOut: true);
            OpenInBrowser(new Uri(ProductOrigin, "/api/v2/auth/logout?next=%2Fauth%2Fcloud-logout").ToString());
        }
        catch (Exception error) when (error is not OutOfMemoryException)
        {
            ShowCloudSignedOut(personSignedOut: true);
        }
        finally
        {
            _personSigningOut = false;
        }
    }

    /// <summary>
    /// Ends this client's own session (§5). The product session is a stateless cookie of the
    /// product server and this client holds no Auth0 refresh token for it, so there is nothing
    /// to revoke; everything of the session in its WebView2 profile is deleted.
    /// </summary>
    private async Task EndCloudProductSessionAsync()
    {
        _cloudProductLoginCancellation?.Cancel();
        await ClearLocalProductSessionAsync();
        ShowCloudSignedOut();
    }

    private async Task ClearLocalProductSessionAsync()
    {
        if (!IsDisposed && _webView.CoreWebView2 is not null)
        {
            await ClearProductCookiesAsync();
            await _webView.CoreWebView2.Profile.ClearBrowsingDataAsync();
        }
        TryDelete(ProductIdentityMarkerPath());
        TryDelete(DeviceAccountPath());
    }

    private static void TryDelete(string path)
    {
        try
        {
            File.Delete(path);
        }
        catch (Exception error) when (error is IOException or UnauthorizedAccessException) { }
    }

    private async Task<bool> HasProductCookiesAsync()
    {
        if (IsDisposed || _webView.CoreWebView2 is null) return false;
        var cookies = await _webView.CoreWebView2.CookieManager.GetCookiesAsync(ProductOrigin.ToString());
        return cookies.Count(c => c.Name is SessionCookie or OriginCookie && c.Expires > DateTime.Now) == 2;
    }

    private static void SaveDeviceAccount(DeviceSignInAccount account)
    {
        try
        {
            BindingWrite(DeviceAccountPath(), JsonSerializer.Serialize(new { subject = account.Subject, label = account.Label }));
        }
        catch (Exception error) when (error is IOException or UnauthorizedAccessException) { }
    }

    private static void BindingWrite(string path, string content)
    {
        Directory.CreateDirectory(Path.GetDirectoryName(path)!);
        var temp = path + ".tmp";
        File.WriteAllText(temp, content);
        File.Move(temp, path, overwrite: true);
    }

    private static DeviceSignInAccount? ReadDeviceAccount()
    {
        try
        {
            if (!File.Exists(DeviceAccountPath())) return null;
            using var document = JsonDocument.Parse(File.ReadAllText(DeviceAccountPath()));
            var subject = document.RootElement.GetProperty("subject").GetString();
            var label = document.RootElement.GetProperty("label").GetString() ?? "";
            return subject is { Length: 43 } ? new DeviceSignInAccount(subject, label) : null;
        }
        catch (Exception error) when (error is IOException or UnauthorizedAccessException or JsonException
            or KeyNotFoundException or InvalidOperationException)
        {
            return null;
        }
    }

    /// <summary>
    /// The account of the session in the WebView: its own record, or, for a session from before
    /// device sign-in (first start after the upgrade), the product's whoami once.
    /// </summary>
    private async Task<DeviceSignInAccount?> CurrentProductAccountAsync(CancellationToken token)
    {
        if (!await HasProductCookiesAsync()) return null;
        if (ReadDeviceAccount() is { } account) return account;
        try
        {
            var cookies = (await _webView.CoreWebView2.CookieManager.GetCookiesAsync(ProductOrigin.ToString()))
                .Where(c => c.Name is SessionCookie or OriginCookie).ToArray();
            using var request = new HttpRequestMessage(HttpMethod.Get, new Uri(ProductOrigin, "/api/v2/whoami"));
            request.Headers.TryAddWithoutValidation("Cookie", string.Join("; ", cookies.Select(c => c.Name + "=" + c.Value)));
            using var response = await _cloudProductHttpClient.SendAsync(request, token);
            if (!response.IsSuccessStatusCode) return null;
            using var whoami = JsonDocument.Parse(await response.Content.ReadAsStringAsync(token));
            var subject = whoami.RootElement.GetProperty("subject").GetString();
            if (string.IsNullOrEmpty(subject)) return null;
            var email = whoami.RootElement.TryGetProperty("email", out var value) && value.ValueKind == JsonValueKind.String
                ? value.GetString() : null;
            account = DeviceSignInAccount.FromIdToken(DeviceSignIn.KombifyIssuer, subject, email, null);
            SaveDeviceAccount(account);
            return account;
        }
        catch (Exception error) when (error is HttpRequestException or JsonException or KeyNotFoundException
            or InvalidOperationException or TaskCanceledException)
        {
            return null;
        }
    }

    private sealed class TechstackDeviceSession(ClientWindow window) : IDeviceSignInSession
    {
        public string Product => DeviceSignInProduct.Techstack;

        public Task<DeviceSignInAccount?> CurrentAccountAsync(CancellationToken cancellationToken) =>
            window.IsDisposed ? Task.FromResult<DeviceSignInAccount?>(null) : window.CurrentProductAccountAsync(cancellationToken);

        public Task EndSessionAsync(CancellationToken cancellationToken) =>
            window.IsDisposed ? Task.CompletedTask : window.EndCloudProductSessionAsync();

        public Task<DeviceSignInAccount?> SilentSignInAsync(string expectedSubject, CancellationToken cancellationToken) =>
            window.IsDisposed
                ? Task.FromResult<DeviceSignInAccount?>(null)
                : window.RunProductLoginAsync(silent: true, cancellationToken, expectedSubject);
    }
}
#endif
