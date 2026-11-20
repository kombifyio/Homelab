using System.Diagnostics;
using System.Text.Json;
using System.Text.Json.Serialization;
using Kombify.Client.Shell;
using Microsoft.Web.WebView2.Core;

namespace Kombify.Homelab.Client;

// The Companion section of the Settings page (platform-pqq2ai; SETTINGS-CONTRACT, AppLink v2). The page
// talks to this window through six fixed, typed messages, gated like the device-context read: the
// current document of the product's own authority, and its revision and token. Every answer goes to
// that document only. The native side holds the AppLink client (Kombify.Client.Shell); no pipe name,
// token, registry path or path of another program reaches the page.
//
//   read          settings.get              the snapshot, or "unavailable" when this installation cannot
//                                           show the Companion (not installed and none carried)
//   write         settings.set (one key)    the snapshot after the change, or a typed refusal; switching on
//                                           while not installed installs the carried Companion first
//   watch         settings.watch            the revision once it rose (held by the program up to 25 s)
//   pick-folder   native folder dialog      the chosen folder, or none
//   open-link     system browser            only a link of the latest snapshot
internal sealed partial class ClientWindow
{
    private const string CompanionPrefix = "kombify.homelab.companion.";
    private const string CompanionResult = CompanionPrefix + "result.v1";
    private const int CompanionMaxMessageChars = 4096;
    private const int CompanionMaxText = 1024;

    private sealed record CompanionRequest(string Type, string RequestId, long DocumentRevision, string DocumentToken);
    private sealed record CompanionWriteRequest(string Type, string RequestId, long DocumentRevision,
        string DocumentToken, string Key, JsonElement Value);
    private sealed record CompanionWatchRequest(string Type, string RequestId, long DocumentRevision,
        string DocumentToken, long After);
    private sealed record CompanionPickRequest(string Type, string RequestId, long DocumentRevision,
        string DocumentToken, string Current);
    private sealed record CompanionLinkRequest(string Type, string RequestId, long DocumentRevision,
        string DocumentToken, string Url);

    private sealed record CompanionDocument(string Source, long Revision, string Token, string RequestId);

    private static readonly JsonSerializerOptions CompanionJson = new()
    {
        PropertyNamingPolicy = JsonNamingPolicy.SnakeCaseLower,
        UnmappedMemberHandling = JsonUnmappedMemberHandling.Disallow,
        // A message with a missing or null field is refused (JsonException), never a null the handlers would meet.
        RespectRequiredConstructorParameters = true,
        RespectNullableAnnotations = true
    };

    private CancellationTokenSource? _companionWatch;
    private string? _companionWatchRequest;
    // What the page may use from the latest snapshot: its links, and the folder values of the path keys.
    private HashSet<string> _companionLinks = [];
    private readonly Dictionary<string, HashSet<string>> _companionFolders = new(StringComparer.Ordinal);
    private string? _companionPickedFolder;
    private ClientActivation? _companionActivation;
    private string? _companionRoute;
    private bool _companionDocumentStarted;

    private void RegisterCompanionBridge()
    {
        FormClosed += (_, _) =>
        {
            _companionWatch?.Cancel();
            _companionActivation?.Dispose();
        };
        _webView.CoreWebView2.NavigationStarting += (_, args) =>
        {
            if (args.Cancel) return;
            _companionWatch?.Cancel();
            _companionPickedFolder = null;
        };
        _webView.CoreWebView2.WebMessageReceived += OnCompanionRequested;
        StartCompanionActivation();
        _ = RegisterCompanionOpenInAppAsync();
        _ = UpgradeInstalledCompanionAsync();
    }

    private void OnCompanionRequested(object? sender, CoreWebView2WebMessageReceivedEventArgs args)
    {
        var source = args.Source;
        var revision = _deviceContextDocumentRevision;
        var token = _deviceContextDocumentToken;
        if (!IsCurrentDeviceContextDocument(source, revision, token)) return;
        string json;
        string type;
        try
        {
            json = args.WebMessageAsJson;
            if (json.Length > CompanionMaxMessageChars) return;
            using var peek = JsonDocument.Parse(json);
            if (peek.RootElement.ValueKind != JsonValueKind.Object
                || !peek.RootElement.TryGetProperty("type", out var typeElement)
                || typeElement.ValueKind != JsonValueKind.String
                || typeElement.GetString() is not { } named || !named.StartsWith(CompanionPrefix, StringComparison.Ordinal)) return;
            type = named;
        }
        catch (Exception error) when (error is JsonException or ArgumentException or InvalidOperationException) { return; }

        try
        {
            switch (type)
            {
                case CompanionPrefix + "read.v1":
                    if (BoundRequest(json, source, revision, token) is { } read) _ = CompanionReadAsync(read);
                    break;
                case CompanionPrefix + "write.v1":
                    if (JsonSerializer.Deserialize<CompanionWriteRequest>(json, CompanionJson) is { } write
                        && Bound(write.RequestId, write.DocumentRevision, write.DocumentToken, source, revision, token) is { } document)
                        _ = CompanionWriteAsync(document, write.Key, write.Value);
                    break;
                case CompanionPrefix + "watch.v1":
                    if (JsonSerializer.Deserialize<CompanionWatchRequest>(json, CompanionJson) is { } watch
                        && watch.After >= 0
                        && Bound(watch.RequestId, watch.DocumentRevision, watch.DocumentToken, source, revision, token) is { } watching)
                        _ = CompanionWatchAsync(watching, (ulong)watch.After);
                    break;
                case CompanionPrefix + "watch-cancel.v1":
                    if (BoundRequest(json, source, revision, token) is { } cancel && cancel.RequestId == _companionWatchRequest)
                        _companionWatch?.Cancel();
                    break;
                case CompanionPrefix + "pick-folder.v1":
                    if (JsonSerializer.Deserialize<CompanionPickRequest>(json, CompanionJson) is { } pick
                        && Bound(pick.RequestId, pick.DocumentRevision, pick.DocumentToken, source, revision, token) is { } picking)
                        BeginInvoke(new Action(() => CompanionPickFolder(picking, pick.Current)));
                    break;
                case CompanionPrefix + "open-link.v1":
                    if (JsonSerializer.Deserialize<CompanionLinkRequest>(json, CompanionJson) is { } link
                        && Bound(link.RequestId, link.DocumentRevision, link.DocumentToken, source, revision, token) is { } opening)
                        CompanionOpenLink(opening, link.Url);
                    break;
            }
        }
        catch (JsonException) { }
    }

    private static CompanionDocument? BoundRequest(string json, string source, long revision, string token)
    {
        var request = JsonSerializer.Deserialize<CompanionRequest>(json, CompanionJson);
        return request is null ? null
            : Bound(request.RequestId, request.DocumentRevision, request.DocumentToken, source, revision, token);
    }

    private static CompanionDocument? Bound(string requestId, long requestRevision, string requestToken,
        string source, long revision, string token) =>
        Guid.TryParse(requestId, out _) && requestRevision == revision && requestToken == token
            ? new CompanionDocument(source, revision, token, requestId) : null;

    private bool CompanionStillShown(CompanionDocument document) =>
        !IsDisposed && _webView.CoreWebView2 is not null && IsCurrentDeviceContextDocument(document.Source, document.Revision, document.Token);

    private void ReplyCompanion(CompanionDocument document, string operation, string status, Action<Dictionary<string, object?>>? fill = null)
    {
        if (!CompanionStillShown(document)) return;
        var message = new Dictionary<string, object?>
        {
            ["type"] = CompanionResult,
            ["request_id"] = document.RequestId,
            ["document_revision"] = document.Revision,
            ["operation"] = operation,
            ["status"] = status,
        };
        fill?.Invoke(message);
        try { _webView.CoreWebView2.PostWebMessageAsJson(JsonSerializer.Serialize(message)); }
        catch (Exception error) when (error is InvalidOperationException or ArgumentException
            or System.Runtime.InteropServices.COMException or ObjectDisposedException) { /* the document is gone */ }
    }

    private void ReplySnapshot(CompanionDocument document, string operation, CompanionSettings settings)
    {
        RememberCompanionSettings(settings);
        using var parsed = JsonDocument.Parse(settings.ToJson());
        var snapshot = parsed.RootElement.Clone();
        ReplyCompanion(document, operation, "ok", message => message["settings"] = snapshot);
    }

    private void RememberCompanionSettings(CompanionSettings settings)
    {
        _companionLinks = new HashSet<string>(settings.Links.Select(link => link.Value), StringComparer.Ordinal);
        _companionFolders.Clear();
        foreach (var value in settings.Values.Where(v => v.Type == CompanionSettingType.Path))
            _companionFolders[value.Key] = new HashSet<string>(
                new[] { value.Value as string, value.Default as string }.OfType<string>(), StringComparer.Ordinal);
    }

    // Reads the snapshot. "unavailable": this installation cannot show the Companion, because the caller
    // may not use the interface, or nothing is installed and nothing installable is carried.
    private async Task CompanionReadAsync(CompanionDocument document)
    {
        try
        {
            if (!CompanionAppLink.IsCallerEligible()) { ReplyCompanion(document, "read", "unavailable"); return; }
            var settings = await ReadCompanionSettingsAsync();
            if (!settings.Installed && !await CompanionInstallableAsync())
            {
                ReplyCompanion(document, "read", "unavailable");
                return;
            }
            ReplySnapshot(document, "read", settings);
        }
        catch (Exception error) when (error is not OutOfMemoryException)
        {
            ReplyCompanion(document, "read", "failed");
        }
    }

    private static Task<bool> CompanionInstallableAsync() => Task.Run(() =>
        CompanionCarry.Payload() is { } payload && CompanionInstaller.CanInstall(payload));

    // An installed Companion older than the carried one, and the carried payload is offered (eligible caller,
    // digest matches). Enumerates Windows Installer: off the UI thread. An unknown state is not an upgrade.
    private static Task<bool> CompanionUpgradableAsync(CompanionPayload payload) => Task.Run(() =>
    {
        try
        {
            return Version.TryParse(payload.Version, out var carried)
                && CompanionInstaller.InstalledVersion() is { } installed && installed < carried
                && CompanionInstaller.CanInstall(payload);
        }
        catch (InvalidOperationException)
        {
            return false;
        }
    });

    // One automatic upgrade attempt per client start (the start check or a refused read, whichever needs it first):
    // a failed one is logged with msiexec's exit code and retried at the next start, never in a loop.
    private static int s_companionUpgradeAttempted;

    // An installed Companion (for this Windows user, whichever kombify client recorded it) that is older than the
    // carried one is upgraded through CompanionInstaller, the same path as a switch-on. Nothing is installed here:
    // with no Companion installed, or the same or a newer one, it does nothing. True when the upgrade succeeded.
    private static async Task<bool> UpgradeInstalledCompanionAsync()
    {
        try
        {
            if (CompanionCarry.Payload() is not { } payload || !await CompanionUpgradableAsync(payload)) return false;
            if (Interlocked.Exchange(ref s_companionUpgradeAttempted, 1) == 1) return false;
            return await InstallCompanionAsync(payload);
        }
        catch (Exception error) when (error is not OutOfMemoryException)
        {
            return false;
        }
    }

    // The settings read. An installed Companion that answers "unsupported_version" is older than this client's
    // AppLink: it is upgraded once and the read is repeated once. Anything else is the read's own answer.
    private static async Task<CompanionSettings> ReadCompanionSettingsAsync()
    {
        CompanionAppLinkException refused;
        try
        {
            return await CompanionAppLink.GetSettingsAsync();
        }
        catch (CompanionAppLinkException error) when (error.Code == CompanionAppLinkError.UnsupportedVersion)
        {
            refused = error;
        }
        if (!await UpgradeInstalledCompanionAsync()) throw refused;
        return await CompanionAppLink.GetSettingsAsync();
    }

    private async Task CompanionWriteAsync(CompanionDocument document, string key, JsonElement value)
    {
        try
        {
            if (key.Length is 0 or > 64 || key.Any(c => !(char.IsAsciiLetterOrDigit(c) || c == '.'))
                || value.ValueKind is not (JsonValueKind.True or JsonValueKind.False or JsonValueKind.String)
                || (value.ValueKind == JsonValueKind.String && value.GetString() is not { Length: <= CompanionMaxText }))
            {
                ReplyRefusal(document, CompanionAppLinkError.InvalidValue);
                return;
            }
            if (value.ValueKind == JsonValueKind.String && IsFolderKey(key) && !FolderAllowed(key, value.GetString()!))
            {
                ReplyRefusal(document, CompanionAppLinkError.InvalidValue);
                return;
            }
            if (key == CompanionSettingKey.Enabled && value.ValueKind == JsonValueKind.True
                && !await EnsureCompanionInstalledAsync())
            {
                ReplyRefusal(document, CompanionAppLinkError.NotAvailable);
                return;
            }
            var result = value.ValueKind == JsonValueKind.String
                ? await CompanionAppLink.SetSettingAsync(CompanionProduct.Homelab, key, value.GetString()!)
                : await CompanionAppLink.SetSettingAsync(CompanionProduct.Homelab, key, value.GetBoolean());
            if (result.Settings is { } settings)
            {
                if (key == CompanionSettingKey.Enabled && settings.Enabled) _ = RegisterCompanionOpenInAppAsync();
                ReplySnapshot(document, "write", settings);
            }
            else
            {
                ReplyRefusal(document, result.Refusal ?? CompanionAppLinkError.NotSaved, result.With);
            }
        }
        catch (Exception error) when (error is not OutOfMemoryException)
        {
            // Whatever failed (the Companion, the state or log directory, the registry), the page gets one answer.
            ReplyRefusal(document, CompanionAppLinkError.NotSaved);
        }
    }

    private void ReplyRefusal(CompanionDocument document, string code, string? with = null) =>
        ReplyCompanion(document, "write", "refused", message =>
        {
            message["error"] = code;
            if (with is not null) message["with"] = with;
        });

    // Switching on while the Companion is not installed installs the carried one first, and while an older
    // one is installed runs the carried MSI over it (its MajorUpgrade, same UpgradeCode: the person's
    // settings and sign-in stay); per user, no elevation, the digest checked on the handle msiexec reads.
    // The same or a newer installed Companion is left alone. Then the caller writes enabled=true.
    private async Task<bool> EnsureCompanionInstalledAsync()
    {
        try
        {
            var current = await ReadCompanionSettingsAsync();
            var carried = CompanionCarry.Payload();
            if (current.Installed && !(carried is not null && await CompanionUpgradableAsync(carried))) return true;
            if (carried is not { } payload) return false;
            // A failed upgrade leaves the installed Companion as it was: the switch goes on with it, as before.
            return await InstallCompanionAsync(payload) || current.Installed;
        }
        catch (Exception error) when (error is not OutOfMemoryException)
        {
            return false;
        }
    }

    // Runs the carried MSI (install or, over an older Companion, its MajorUpgrade) and logs the result.
    private static async Task<bool> InstallCompanionAsync(CompanionPayload payload)
    {
        var logs = Path.Combine(ClientConfig.StateDirectory(), "logs");
        Directory.CreateDirectory(logs);
        var install = await CompanionInstaller.InstallAsync(payload, Path.Combine(logs, "companion-install.log"));
        // The client's own log keeps the result code and msiexec's exit code only.
        AppendRuntimeLog(Path.Combine(logs, "companion.log"),
            $"[{DateTimeOffset.Now:u}] companion install: {install.Code} exit={install.ExitCode?.ToString() ?? "-"}");
        return install.Registered;
    }

    private static bool IsFolderKey(string key) => key == CompanionSettingKey.ScreenshotFolder;

    // A folder the page writes is the one the person just picked in the native dialog, or a value the
    // Companion itself reported for that key (its current folder or its default).
    private bool FolderAllowed(string key, string folder) =>
        string.Equals(folder, _companionPickedFolder, StringComparison.OrdinalIgnoreCase)
        || (_companionFolders.TryGetValue(key, out var known) && known.Contains(folder));

    private async Task CompanionWatchAsync(CompanionDocument document, ulong after)
    {
        _companionWatch?.Cancel();
        using var cancellation = new CancellationTokenSource();
        _companionWatch = cancellation;
        _companionWatchRequest = document.RequestId;
        try
        {
            var revision = await CompanionAppLink.WatchSettingsAsync(after, cancellation.Token);
            if (revision > 9007199254740991UL) { ReplyCompanion(document, "watch", "failed"); return; }
            ReplyCompanion(document, "watch", "ok", message => message["revision"] = (long)revision);
        }
        catch (OperationCanceledException) { }
        catch (Exception error) when (error is not OutOfMemoryException) { ReplyCompanion(document, "watch", "failed"); }
        finally
        {
            if (ReferenceEquals(_companionWatch, cancellation))
            {
                _companionWatch = null;
                _companionWatchRequest = null;
            }
        }
    }

    private void CompanionPickFolder(CompanionDocument document, string current)
    {
        if (!CompanionStillShown(document)) return;
        string? chosen = null;
        try
        {
            using var dialog = new FolderBrowserDialog { ShowNewFolderButton = true };
            if (current.Length is > 0 and <= 1024 && Path.IsPathFullyQualified(current) && Directory.Exists(current))
                dialog.InitialDirectory = current;
            if (dialog.ShowDialog(this) == DialogResult.OK && Path.IsPathFullyQualified(dialog.SelectedPath))
                chosen = dialog.SelectedPath;
        }
        catch (Exception error) when (error is not OutOfMemoryException)
        {
            ReplyCompanion(document, "pick-folder", "failed");
            return;
        }
        if (!CompanionStillShown(document)) return;
        _companionPickedFolder = chosen;
        ReplyCompanion(document, "pick-folder", "ok", message => message["path"] = chosen);
    }

    // Only a link of the latest snapshot (an https address the Companion reported) opens in the system browser.
    private void CompanionOpenLink(CompanionDocument document, string url)
    {
        if (url.Length > CompanionMaxText || !_companionLinks.Contains(url)
            || !Uri.TryCreate(url, UriKind.Absolute, out var uri) || uri.Scheme != Uri.UriSchemeHttps
            || uri.UserInfo.Length != 0)
        {
            ReplyCompanion(document, "open-link", "failed");
            return;
        }
        try
        {
            Process.Start(new ProcessStartInfo { FileName = uri.AbsoluteUri, UseShellExecute = true });
            ReplyCompanion(document, "open-link", "ok");
        }
        catch (Exception error) when (error is not OutOfMemoryException)
        {
            ReplyCompanion(document, "open-link", "failed");
        }
    }

    // ── URL activation: kombify-homelab://open/?route=%2Fsettings%2Fcompanion ─────────────────────

    // A later start (Windows passes the URL) hands its route to this instance (Program.Main); this instance
    // brings its window to the front and opens Settings > Companion.
    private void StartCompanionActivation()
    {
        try
        {
            _companionActivation = ClientActivation.Listen(CompanionProduct.Homelab, DesktopEdition.ActivationScheme,
                route =>
                {
                    try { BeginInvoke(new Action(() => ApplyCompanionActivation(route))); }
                    catch (InvalidOperationException) { /* the window is closing */ }
                });
        }
        catch (CompanionAppLinkException)
        {
            // Not eligible (elevated), or another instance already holds the name: nothing to listen for.
        }
    }

    [System.Runtime.InteropServices.DllImport("user32.dll")]
    private static extern bool ShowWindow(IntPtr window, int command);

    private void ApplyCompanionActivation(string? route)
    {
        if (IsDisposed) return;
        // SW_RESTORE returns a minimized window to the state it had (maximized stays maximized).
        if (WindowState == FormWindowState.Minimized) _ = ShowWindow(Handle, 9);
        Activate();
        if (route == CompanionOpenInApp.SettingsRoute) OpenCompanionSettings();
    }

    // The route this window was started with (Program.Main) waits for the first document of the product.
    private void OpenCompanionSettings()
    {
        if (!_companionDocumentStarted) { _companionRoute = CompanionOpenInApp.SettingsRoute; return; }
        if (_webView.CoreWebView2 is null) return;
        _webView.CoreWebView2.Navigate(CompanionSettingsUrl());
    }

    private string CompanionSettingsUrl()
    {
        var selected = new Uri(_boundServerProfile?.BaseUrl ?? _config.InitialUrl());
        return selected.GetLeftPart(UriPartial.Authority) + "/settings?tab=companion";
    }

    // The product's first document: the Companion's settings when the start came from its URL.
    private string InitialDocumentUrl()
    {
        _companionDocumentStarted = true;
        if (_companionRoute != CompanionOpenInApp.SettingsRoute) return _boundServerProfile?.BaseUrl ?? _config.InitialUrl();
        _companionRoute = null;
        return CompanionSettingsUrl();
    }

    // The client's own URL scheme, once a Companion exists to use it (AppLink v2: Windows URL activation).
    private static async Task RegisterCompanionOpenInAppAsync()
    {
        try
        {
            if (!CompanionAppLink.IsCallerEligible() || Environment.ProcessPath is not { } executable) return;
            if (!(await CompanionAppLink.DiscoverAsync()).Installed) return;
            CompanionOpenInApp.Register(CompanionProduct.Homelab, DesktopEdition.ActivationScheme, executable,
                Program.ActivationSwitch);
        }
        catch (Exception error) when (error is not OutOfMemoryException)
        {
            // The Companion is not reachable now (or the registry refused); the next start or switch-on registers again.
        }
    }

    // Set from the activation URL this process was started with (Program.Main).
    internal void SetStartRoute(string? route)
    {
        if (route == CompanionOpenInApp.SettingsRoute) _companionRoute = route;
    }
}
