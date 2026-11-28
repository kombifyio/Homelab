using System.Diagnostics;
using System.Drawing;
using System.Net;
using System.Net.Sockets;
using System.Security.Cryptography;
using System.Text;
using System.Text.Json;
using System.Text.Json.Serialization;
using Microsoft.Web.WebView2.Core;
using Microsoft.Web.WebView2.WinForms;
using Kombify.Client.Shell;

namespace Kombify.Homelab.Client;

internal static class Program
{
    // Held for the life of the process: its existence tells a later start that a client is running.
    private static Mutex? s_runningInstance;

    // The only switch of the client's URL handler: `"exe" --activate "<url>"`.
    internal const string ActivationSwitch = "--activate";
    internal const string RestartAfterSwitch = "--restart-after";

    [STAThread]
    private static void Main(string[] args)
    {
        // Windows starts the client for kombify-homelab://open/?route=... as `exe --activate <url>`.
        // Exactly the one argument after --activate is the URL; nothing else on that command line is read
        // (a URL with an unencoded quote or space must not become switches such as --url or --runtime-exe),
        // and the configuration loads as for a start without arguments.
        // "Restart to update" starts a new client with `--restart-after <pid>` and closes: the new client
        // waits for that one (and its local runtime) to end, so the staged update below can apply.
        if (args.Length >= 2 && args[0] == RestartAfterSwitch)
        {
            WaitForPreviousClient(args[1]);
            args = [];
        }
        var activation = args.Length >= 1 && args[0] == ActivationSwitch;
        var activationRoute = activation && args.Length >= 2
            ? ClientActivation.RouteFromArguments([args[1]], DesktopEdition.ActivationScheme) : null;
        var mutexName = @"Local\kombify-techstack-client-" + DesktopEdition.ActivationScheme;
        // A running client gets the route and this start exits. Without one (or while an elevated instance
        // holds the name, which this process may not open) it starts normally and opens the route itself.
        if (activation)
        {
            var running = false;
            try
            {
                running = Mutex.TryOpenExisting(mutexName, out var existing);
                existing?.Dispose();
            }
            catch (Exception error) when (error is UnauthorizedAccessException or IOException or WaitHandleCannotBeOpenedException) { }
            if (running && ClientActivation.HandOffAsync(CompanionProduct.Homelab, DesktopEdition.ActivationScheme, activationRoute)
                .GetAwaiter().GetResult()) return;
        }
        try { s_runningInstance = new Mutex(false, mutexName); }
        catch (Exception error) when (error is UnauthorizedAccessException or IOException or WaitHandleCannotBeOpenedException) { }
        var config = ClientConfig.Load(activation ? [] : args);
        // A verified staged update is applied before anything else starts.
        if (ClientUpdater.TryApplyStagedUpdate(config, out var startupBlockReason))
        {
            return;
        }
        ApplicationConfiguration.Initialize();
#if CLOUD_DESKTOP
        if (args.Contains("--connect-enroll", StringComparer.Ordinal))
        {
            MessageBox.Show(CloudLoginText.Get("ConnectSignInRequired"), "kombify Connect",
                MessageBoxButtons.OK, MessageBoxIcon.Warning);
            return;
        }
#endif
        // A blocked recovery keeps the runtime stopped; the window says so and "Try again" starts the client anew.
        var window = new ClientWindow(config, startupBlockReason);
        window.SetStartRoute(activationRoute);
        Application.Run(window);
    }

    private static void WaitForPreviousClient(string processId)
    {
        if (!int.TryParse(processId, out var id) || id == Environment.ProcessId) return;
        try
        {
            using var previous = Process.GetProcessById(id);
            if (previous.ProcessName.Equals(Path.GetFileNameWithoutExtension(Environment.ProcessPath), StringComparison.OrdinalIgnoreCase))
                previous.WaitForExit(60000);
        }
        catch (Exception error) when (error is ArgumentException or InvalidOperationException or System.ComponentModel.Win32Exception) { }
    }

    // Starts this client again after the current one closed (update restart, retry after a blocked recovery).
    internal static void StartAgainAfterExit()
    {
        var path = Environment.ProcessPath;
        if (string.IsNullOrEmpty(path)) return;
        var start = new ProcessStartInfo(path) { UseShellExecute = false };
        start.ArgumentList.Add(RestartAfterSwitch);
        start.ArgumentList.Add(Environment.ProcessId.ToString(System.Globalization.CultureInfo.InvariantCulture));
        using var _ = Process.Start(start);
    }
}

internal sealed record ClientConfig
{
    private const string LegacyLocalOnboardingUrl = "http://127.0.0.1:5260/client/onboarding?client=windows";
    private const string DefaultLocalOnboardingUrl = "http://127.0.0.1:5260/client/local?client=windows";
    private static readonly ClientStatePaths StatePaths = new("techstack", DesktopEdition.Name, DesktopEdition.Channel,
        existingDefaultDirectory: DesktopEdition.IsLocal ? Path.Combine(
            Environment.GetFolderPath(Environment.SpecialFolder.LocalApplicationData), "kombify", "techstack-client") : null,
        legacyOverridePrefixes: DesktopEdition.IsLocal
            ? ["techstack-client-contract-", "techstack-client-startup-", "techstack-client-smoke-"] : null);

    public string Mode { get; init; } = DesktopEdition.Name;
#if CLOUD_DESKTOP
    public string CloudUrl { get; init; } = "https://kombify.io/device";
    public string CloudDeviceUrl { get; init; } = "https://kombify.io/device";
    public string CloudUiUrl { get; init; } = "https://homelab.kombify.io/login?manual=1&client=windows";
    public string CloudDeviceCodeEndpoint { get; init; } =
        "https://app.kombify.io/api/v1/tools/auth/device-code";
    public string CloudDevicePollEndpoint { get; init; } =
        "https://app.kombify.io/api/v1/tools/auth/device-code/poll";
    public string ToolName { get; init; } = "stack";
#endif
    public string LocalUiUrl { get; init; } = "http://127.0.0.1:5260/";
    public string LocalOnboardingUrl { get; init; } = DefaultLocalOnboardingUrl;
    public string ServerUrl { get; init; } = "";
    public string RuntimeExecutablePath { get; init; } = "";
    public string RuntimeDataDir { get; init; } = Path.Combine(StateDirectory(), "runtime");
    public int RuntimeStartupTimeoutSeconds { get; init; } = 120;
    public bool AutoStartRuntime { get; init; } = true;
    // Signed update channel (DESKTOP-FOUNDATION-STANDARD.md §5.7). The
    // The verifier's trusted signing key and edition cannot be configured.
    // AutoUpdate=false opts out; Cloud always uses its own signed beta feed.
    public string UpdateManifestUrl { get; init; } = DesktopEdition.UpdateManifestUrl;
    public string UpdateChannel { get; init; } = DesktopEdition.Channel;
    public string UpdateMinSupportedVersion { get; init; } = "";
    public bool AutoUpdate { get; init; } = true;
#if CLOUD_DESKTOP
    // kombify Connect installation enrollment (CONNECT-CONTRACT-STANDARD section 5).
    // ConnectAuth0ClientId is the public Auth0 native application
    // "kombify Homelab Windows" (PKCE, loopback /oauth/callback on 63690-63694).
    public string ConnectOrigin { get; init; } = "https://connect.kombify.io/";
    public string ConnectAuth0Issuer { get; init; } = "https://login.kombify.io/";
    public string ConnectAuth0ClientId { get; init; } = "D8srdyeDyzxIpuqRYo4lwmEDaUKFE3X8";
    public string ConnectAuth0Audience { get; init; } = "https://api.kombify.io";
#endif

    public static ClientConfig Load(string[] args)
    {
        var config = Read(ConfigPath()) ?? new ClientConfig();
        if (!DesktopEdition.IsLocal && string.IsNullOrWhiteSpace(config.UpdateManifestUrl))
            config = config with { UpdateManifestUrl = DesktopEdition.UpdateManifestUrl };
        if (config.LocalOnboardingUrl.Equals(LegacyLocalOnboardingUrl, StringComparison.OrdinalIgnoreCase))
        {
            config = config with { LocalOnboardingUrl = DefaultLocalOnboardingUrl };
        }

        for (var i = 0; i < args.Length; i++)
        {
#if !CLOUD_DESKTOP
            if (args[i] is "--cloud-ui-url" or "--connect-enroll")
                throw new InvalidOperationException("The Standalone installer does not contain kombify Cloud or Connect enrollment.");
#endif
            if (args[i] == "--url" && i + 1 < args.Length)
            {
                config = config with { Mode = "server", ServerUrl = args[++i] };
                continue;
            }

            if (args[i] == "--mode" && i + 1 < args.Length)
            {
                config = config with { Mode = args[++i] };
                continue;
            }

#if CLOUD_DESKTOP
            if (args[i] == "--cloud-ui-url" && i + 1 < args.Length)
            {
                config = config with { Mode = "cloud", CloudUiUrl = args[++i] };
                continue;
            }
#endif

            if (args[i] == "--local-ui-url" && i + 1 < args.Length)
            {
                config = config with { Mode = "local", LocalUiUrl = args[++i] };
                continue;
            }

            if (args[i] == "--local-onboarding-url" && i + 1 < args.Length)
            {
                config = config with { Mode = "local", LocalOnboardingUrl = args[++i] };
                continue;
            }

            if (args[i] == "--runtime-exe" && i + 1 < args.Length)
            {
                config = config with { RuntimeExecutablePath = args[++i] };
                continue;
            }

            if (args[i] == "--runtime-data-dir" && i + 1 < args.Length)
            {
                config = config with { RuntimeDataDir = args[++i] };
                continue;
            }

            if (args[i] == "--runtime-startup-timeout-seconds" && i + 1 < args.Length
                && int.TryParse(args[++i], out var timeoutSeconds))
            {
                config = config with { RuntimeStartupTimeoutSeconds = timeoutSeconds };
                continue;
            }

            if (args[i] == "--no-runtime-start")
            {
                config = config with { AutoStartRuntime = false };
            }
        }

        if (DesktopEdition.IsLocal && config.Mode.Equals("cloud", StringComparison.OrdinalIgnoreCase))
            throw new InvalidOperationException("The Standalone installer cannot connect to kombify Cloud.");
        if (!DesktopEdition.IsLocal && !config.Mode.Equals("cloud", StringComparison.OrdinalIgnoreCase))
            throw new InvalidOperationException("The Connected installer cannot select a self-hosted product authority.");
        if (config.Mode is not ("local" or "server" or "cloud"))
            throw new InvalidOperationException("The requested desktop connection mode is unsupported.");
        if (!config.UpdateChannel.Equals(DesktopEdition.Channel, StringComparison.OrdinalIgnoreCase))
            throw new InvalidOperationException("The update channel does not match this desktop installer.");
        ClientAuthority.Validate(config);
        return config;
    }

    public string InitialUrl()
    {
        if (Mode.Equals("server", StringComparison.OrdinalIgnoreCase) && IsHttp(ServerUrl))
        {
            return ServerUrl;
        }

#if CLOUD_DESKTOP
        if (Mode.Equals("cloud", StringComparison.OrdinalIgnoreCase) && IsHttp(CloudUiUrl))
        {
            return CloudUiUrl;
        }

        if (Mode.Equals("cloud", StringComparison.OrdinalIgnoreCase) && IsHttp(CloudUrl))
        {
            return CloudUrl;
        }
#endif

        return IsHttp(LocalOnboardingUrl) ? LocalOnboardingUrl : LocalUiUrl;
    }

    public static void ValidateProfileAuthority(ClientConnectionProfile profile)
    {
        if (DesktopEdition.IsLocal && profile.DeploymentMode == "cloud")
            throw new ClientProfileException("The Standalone installer cannot bind to a Cloud data authority.");
        if (!DesktopEdition.IsLocal && profile.DeploymentMode != "cloud")
            throw new ClientProfileException("The Connected installer cannot bind to a self-hosted data authority.");
    }

    public Uri? LocalUiUri()
    {
        return IsHttp(LocalUiUrl) ? new Uri(LocalUiUrl) : null;
    }

    public string LocalOrigin()
    {
        var uri = LocalUiUri();
        if (uri is null)
        {
            return "http://127.0.0.1:5260";
        }

        return new UriBuilder(uri.Scheme, uri.Host, uri.Port).Uri.ToString().TrimEnd('/');
    }

    public string RuntimeLogPath()
    {
        return Path.Combine(RuntimeDataDir, "homelab-runtime.log");
    }

    public static string ConfigPath()
    {
        return Path.Combine(
            StateDirectory(),
            "client.json");
    }

    public static string StateDirectory()
    {
        var configured = Environment.GetEnvironmentVariable("HOMELAB_CLIENT_STATE_DIR")?.Trim();
        return StatePaths.ResolveDirectory(configured);
    }

    public static string CredentialTarget(string baseTarget)
    {
        return StatePaths.CredentialTargetFor(StateDirectory(), baseTarget);
    }

    private static ClientConfig? Read(string path)
    {
        if (!File.Exists(path))
        {
            return null;
        }

        try
        {
            return JsonSerializer.Deserialize<ClientConfig>(
                File.ReadAllText(path),
                new JsonSerializerOptions { PropertyNameCaseInsensitive = true });
        }
        catch
        {
            return null;
        }
    }

    private static bool IsHttp(string value)
    {
        return Uri.TryCreate(value, UriKind.Absolute, out var uri)
            && (uri.Scheme == Uri.UriSchemeHttp || uri.Scheme == Uri.UriSchemeHttps);
    }
}

#if CLOUD_DESKTOP
internal sealed record CloudDeviceCodeResponse
{
    [JsonPropertyName("device_code")]
    public string DeviceCode { get; init; } = "";

    [JsonPropertyName("user_code")]
    public string UserCode { get; init; } = "";

    [JsonPropertyName("verification_uri")]
    public string VerificationUri { get; init; } = "";

    [JsonPropertyName("verification_uri_complete")]
    public string VerificationUriComplete { get; init; } = "";

    [JsonPropertyName("expires_in")]
    public int ExpiresIn { get; init; } = 900;

    [JsonPropertyName("interval")]
    public int Interval { get; init; } = 5;
}

internal sealed record CloudToolTokenResponse
{
    [JsonPropertyName("access_token")]
    public string AccessToken { get; init; } = "";

    [JsonPropertyName("refresh_token")]
    public string RefreshToken { get; init; } = "";

    [JsonPropertyName("expires_at")]
    public string ExpiresAt { get; init; } = "";

    [JsonPropertyName("user_id")]
    public string UserId { get; init; } = "";
}

internal sealed record CloudEntitlementResponse
{
    [JsonPropertyName("tool")]
    public string Tool { get; init; } = "";

    [JsonPropertyName("expires_at")]
    public string? ExpiresAt { get; init; }
}

internal sealed record CloudAuthorizationResponse
{
    [JsonPropertyName("token")]
    public CloudToolTokenResponse Token { get; init; } = new();

    [JsonPropertyName("entitlements")]
    public CloudEntitlementResponse? Entitlements { get; init; }
}
#endif

internal sealed partial class ClientWindow : Form
{
    private static readonly JsonSerializerOptions JsonOptions = new(JsonSerializerDefaults.Web);
    private static readonly object RuntimeLogLock = new();
    private const string LocalDeviceTokenEnv = "HOMELAB_LOCAL_DEVICE_TOKEN";
    private const string LocalRuntimeSessionCredentialTarget = "kombify/techstack/local/runtime-session-secret";
    private const string LocalDeviceCredentialTarget = "kombify/techstack/local/device-session-token";
    private const string LocalRuntimeEncryptionKeyCredentialTarget = "kombify/techstack/local/runtime-encryption-key";

    private readonly ClientConfig _config;
    private readonly WebView2 _webView = new() { Dock = DockStyle.Fill };
    // Discovery and credential-bearing requests must not follow an origin's redirect.
    private readonly HttpClient _httpClient = ClientAuthority.CreateHttpClient();
#if CLOUD_DESKTOP
    private CancellationTokenSource? _cloudLoginCancellation;
#endif
    private SupervisedProcess? _runtimeProcess;
    private ClientConnectionProfile? _boundServerProfile;
    private readonly string? _startupBlockReason;
    private bool _productOpened;

    public ClientWindow(ClientConfig config, string? startupBlockReason = null)
    {
        _config = config;
        _startupBlockReason = startupBlockReason;
        Text = ShellText.ProductName;
        MinimumSize = new Size(980, 680);
        StartPosition = FormStartPosition.CenterScreen;
        WindowState = FormWindowState.Maximized;
        // The window colour behind the page follows the Windows app theme, so a dark start does not flash light.
        BackColor = ShellText.PrefersDark() ? Color.FromArgb(23, 25, 28) : Color.FromArgb(247, 249, 251);

        var icon = LoadIcon();
        if (icon is not null)
        {
            Icon = icon;
        }

        Controls.Add(_webView);
        Shown += async (_, _) => await StartAsync();
        FormClosed += (_, _) =>
        {
#if CLOUD_DESKTOP
            _cloudLoginCancellation?.Cancel();
            _cloudProductLoginCancellation?.Cancel();
#endif
            StopStartedLocalRuntime();
        };
    }

#if CLOUD_DESKTOP
    // "kombify Connect…" in the window's system menu (Alt+Space / title-bar menu)
    // is the settings entry point for Connect enrollment and confirmation.
    private const int WmSysCommand = 0x0112;
    private const int ConnectMenuCommand = 0x4B40; // low four bits must be zero for WM_SYSCOMMAND ids
    private bool _connectSettingsOpening;

    [System.Runtime.InteropServices.DllImport("user32.dll")]
    private static extern IntPtr GetSystemMenu(IntPtr hWnd, bool bRevert);

    [System.Runtime.InteropServices.DllImport("user32.dll", CharSet = System.Runtime.InteropServices.CharSet.Unicode)]
    private static extern bool AppendMenu(IntPtr hMenu, uint uFlags, uint uIDNewItem, string lpNewItem);

    protected override void OnHandleCreated(EventArgs e)
    {
        base.OnHandleCreated(e);
        var menu = GetSystemMenu(Handle, false);
        AppendMenu(menu, 0x800 /* MF_SEPARATOR */, 0, "");
        AppendMenu(menu, 0 /* MF_STRING */, ConnectMenuCommand, "kombify Connect…");
    }

    protected override void WndProc(ref Message m)
    {
        if (m.Msg == WmSysCommand && (m.WParam.ToInt64() & 0xFFF0) == ConnectMenuCommand)
        {
            _ = OpenConnectSettingsAsync();
            return;
        }

        base.WndProc(ref m);
    }

    private async Task OpenConnectSettingsAsync()
    {
        if (_connectSettingsOpening) return;
        _connectSettingsOpening = true;
        try
        {
            var identity = await CurrentConnectIdentityAsync(CancellationToken.None);
            if (IsDisposed) return;
            using var settings = new ConnectSettingsForm(_config, identity, CurrentConnectIdentityAsync);
            settings.ShowDialog(this);
        }
        catch (Exception)
        {
            if (!IsDisposed)
                MessageBox.Show(CloudLoginText.Get("ConnectAccountRequired"), "kombify Connect",
                    MessageBoxButtons.OK, MessageBoxIcon.Warning);
        }
        finally
        {
            _connectSettingsOpening = false;
        }
    }
#endif

    private async Task StartAsync()
    {
        try
        {
            var userData = Path.Combine(ClientConfig.StateDirectory(),
                DesktopEdition.IsLocal ? "webview2" : "webview2-cloud-product-v1");
            Directory.CreateDirectory(userData);

            var env = await CoreWebView2Environment.CreateAsync(null, userData);
            await _webView.EnsureCoreWebView2Async(env);
            _webView.CoreWebView2.NavigationStarting += OnNavigationStarting;
            if (!DesktopEdition.IsLocal)
            {
                _webView.CoreWebView2.NavigationCompleted += (_, navigation) =>
                {
                    // WebView construction alone does not acknowledge a Cloud
                    // update. An unavailable service leaves its cursor pending.
                    if (navigation.IsSuccess && navigation.HttpStatusCode is >= 200 and < 400 &&
                        _webView.CoreWebView2.Source is { } source &&
                        Uri.TryCreate(source, UriKind.Absolute, out var document) &&
                        ClientAuthority.AllowsProductNavigation(document, _config.InitialUrl()))
                        ClientUpdater.CompletePendingUpdate(_config, true, () => { });
                };
                _webView.CoreWebView2.ProcessFailed += (_, _) =>
                {
                    if (ClientUpdater.CompletePendingUpdate(_config, false, () => { })) Close();
                };
            }
            RegisterDeviceContextBridge();
            RegisterShellBridge();

            if (_startupBlockReason is not null)
            {
                // Recovery after an update is not finished: nothing starts, the person can try again.
                ShowFailure(ShellText.Get("UpdateBlockedTitle"), _startupBlockReason, "", RestartClient);
                return;
            }
            await OpenProductAsync();
        }
        catch (Exception ex)
        {
            if (!DesktopEdition.IsLocal && ClientUpdater.CompletePendingUpdate(_config, false, () => { }))
            {
                Close();
                return;
            }
            // Without WebView2 there is no page to draw on; the window still says it in the person's language.
            Controls.Clear();
            Controls.Add(new Label
            {
                Dock = DockStyle.Fill,
                TextAlign = ContentAlignment.MiddleCenter,
                ForeColor = ShellText.PrefersDark() ? Color.FromArgb(240, 241, 243) : Color.FromArgb(28, 30, 33),
                Text = $"{ShellText.Get("StartFailedTitle")}\n\n{ex.Message}",
            });
        }
    }

    // Starts (or, after "Try again", starts again) what this installation shows: the local runtime, the
    // person's own server or the Cloud product. Every failure ends on the shell's failure page.
    private async Task OpenProductAsync()
    {
        if (IsLocalMode())
        {
            NavigateHtml(RenderProgress(ShellText.Get("StartingTitle")));
            var runtimeReady = await EnsureLocalRuntimeAsync();
            if (!runtimeReady)
            {
                if (ClientUpdater.CompletePendingUpdate(_config, false, StopStartedLocalRuntime))
                    Close();
                return;
            }
            if (!await BootstrapLocalDeviceSessionAsync())
            {
                if (ClientUpdater.CompletePendingUpdate(_config, false, StopStartedLocalRuntime))
                {
                    Close();
                    return;
                }
                ShowLocalStartFailure(LocalRuntimeMessages.IdentityVerificationFailed, _config.RuntimeLogPath());
                return;
            }
            if (ClientUpdater.CompletePendingUpdate(_config, true, StopStartedLocalRuntime))
            {
                Close();
                return;
            }
        }
        else if (DesktopEdition.IsLocal && ClientUpdater.CompletePendingUpdate(_config, true, () => { }))
        {
            Close();
            return;
        }

        if (IsServerMode())
        {
            NavigateHtml(RenderProgress(ShellText.Get("ConnectingTitle")));
            _boundServerProfile = await BindServerProfileAsync();
            if (_boundServerProfile is null)
            {
                return;
            }
        }

        var launchUrl = InitialDocumentUrl();
        var firstOpen = !_productOpened;
        _productOpened = true;
#if CLOUD_DESKTOP
        if (firstOpen) launchUrl = await StartDeviceSignInAsync(launchUrl);
#endif
        _webView.CoreWebView2.Navigate(launchUrl);
#if CLOUD_DESKTOP
        if (firstOpen && _deviceSignIn is { } deviceSignIn) _ = deviceSignIn.StartAsync();
#endif
        if (firstOpen) _ = CheckForUpdateAsync();
    }

    private bool IsLocalMode()
    {
        return _config.Mode.Equals("local", StringComparison.OrdinalIgnoreCase);
    }

    private bool IsServerMode()
    {
        return _config.Mode.Equals("server", StringComparison.OrdinalIgnoreCase);
    }

    private async Task<ClientConnectionProfile?> BindServerProfileAsync()
    {
        try
        {
            var configuredEndpoint = ClientConnectionProfileValidator.NormalizeConfiguredEndpoint(_config.ServerUrl);
            var discoveryEndpoint = new Uri(configuredEndpoint, "/.well-known/kombify-client");
            using var request = new HttpRequestMessage(HttpMethod.Get, discoveryEndpoint);
            request.Headers.Accept.ParseAdd("application/json");
            using var response = await _httpClient.SendAsync(request, HttpCompletionOption.ResponseHeadersRead);
            if (!response.IsSuccessStatusCode)
            {
                ShowServerBindingFailure(
                    $"The server returned {(int)response.StatusCode} {response.StatusCode} from client discovery at {discoveryEndpoint}. "
                    + "Check the server's public URL and native OIDC configuration.");
                return null;
            }
            if (!string.Equals(response.Content.Headers.ContentType?.MediaType, "application/json", StringComparison.OrdinalIgnoreCase))
            {
                throw new ClientProfileException("The discovery endpoint did not return application/json.");
            }
            if (response.Content.Headers.ContentLength is > 65536)
            {
                throw new ClientProfileException("The discovery response exceeds the 64 KiB client-profile limit.");
            }

            var body = await response.Content.ReadAsStringAsync();
            if (Encoding.UTF8.GetByteCount(body) > 65536)
            {
                throw new ClientProfileException("The discovery response exceeds the 64 KiB client-profile limit.");
            }
            var profile = ClientConnectionProfileValidator.ParseAndValidate(body, configuredEndpoint);
            ClientConfig.ValidateProfileAuthority(profile);
            var path = Path.Combine(ClientConfig.StateDirectory(), "connection-profile.json");
            ClientConnectionProfileValidator.PersistPublicProfile(profile, path);
            return profile;
        }
        catch (Exception error) when (error is ClientProfileException or HttpRequestException or TaskCanceledException)
        {
            ShowServerBindingFailure(
                $"{error.Message} Verify the HTTPS endpoint ({_config.ServerUrl}) or use a fresh pairing link from the server's owner.");
            return null;
        }
    }

    private void ShowServerBindingFailure(string detail)
    {
        ShowFailure(ShellText.Get("ServerFailedTitle"), detail, "", RetryOpenProduct);
    }

    private void ShowLocalStartFailure(string detail, string logPath)
    {
        ShowFailure(ShellText.Get("StartFailedTitle"), detail, RuntimeLogExtraHtml(logPath), RetryOpenProduct);
    }

    private async Task<bool> EnsureLocalRuntimeAsync()
    {
        if (await LocalRuntimeReadyAsync(TimeSpan.FromSeconds(2)))
        {
            return true;
        }

        if (!_config.AutoStartRuntime)
        {
            ShowFailure(ShellText.Get("StartFailedTitle"),
                $"Automatic runtime start is switched off (autoStartRuntime=false) and no runtime answers at {_config.LocalOrigin()}.",
                "", RetryOpenProduct);
            return false;
        }

        var runtimeExe = ResolveRuntimeExecutablePath();
        if (string.IsNullOrWhiteSpace(runtimeExe))
        {
            ShowLocalStartFailure(
                "homelab.exe was not found next to kombify-homelab-client.exe; the installation is incomplete. Repair or reinstall it.",
                _config.RuntimeLogPath());
            return false;
        }

        Directory.CreateDirectory(_config.RuntimeDataDir);
        var logPath = _config.RuntimeLogPath();
        Directory.CreateDirectory(Path.GetDirectoryName(logPath) ?? _config.RuntimeDataDir);
        AppendRuntimeLog(logPath, $"[{DateTimeOffset.Now:u}] starting local runtime: {runtimeExe}");

        try
        {
            var start = new ProcessStartInfo
            {
                FileName = runtimeExe,
                WorkingDirectory = Path.GetDirectoryName(runtimeExe) ?? AppContext.BaseDirectory,
                UseShellExecute = false,
                CreateNoWindow = true,
                RedirectStandardOutput = true,
                RedirectStandardError = true,
            };
            start.Environment["HOMELAB_LISTEN_ADDR"] = RuntimeListenAddr();
            start.Environment["HOMELAB_GRPC_ADDR"] = RuntimeGrpcAddr();
            start.Environment["HOMELAB_DATA_DIR"] = _config.RuntimeDataDir;
            start.Environment["HOMELAB_RUNTIME_LOG_PATH"] = Path.Combine(_config.RuntimeDataDir, "runtime-logs.jsonl");
            start.Environment["HOMELAB_PUBLIC_ORIGIN"] = _config.LocalOrigin();
            start.Environment["HOMELAB_CORS_ORIGINS"] = _config.LocalOrigin();
            start.Environment["HOMELAB_ENV"] = "local";
            start.Environment["KOMBIFY_EDITION"] = "selfhost-oss";
            start.Environment["HOMELAB_EMBEDDED_POSTGRES"] = "1";
            start.Environment["HOMELAB_SHUTDOWN_ON_STDIN_CLOSE"] = "1";
            start.Environment["HOMELAB_EMBEDDED_POSTGRES_BUNDLE_DIR"] =
                Path.Combine(Path.GetDirectoryName(runtimeExe) ?? AppContext.BaseDirectory, "postgres");
            start.Environment["HOMELAB_EMBEDDED_POSTGRES_START_TIMEOUT_SECONDS"] = "120";
            start.Environment["HOMELAB_V2_SESSION_SECRET"] = EnsureRuntimeSessionSecret();
            // Wallet, backup and StackKit custody fail closed without this key.
            start.Environment["HOMELAB_ENCRYPTION_KEY"] = EnsureRuntimeEncryptionKey();
            start.Environment["HOMELAB_V2_SESSION_AUDIENCE"] = "homelab-local";
            start.Environment["HOMELAB_V2_DEFAULT_TENANT_ID"] = "default";
            start.Environment["HOMELAB_ALLOW_UNSIGNED_WORKER_TOKEN"] = "1";
			start.Environment[LocalDeviceTokenEnv] = EnsureLocalDeviceToken();
			var runtimeDirectory = Path.GetDirectoryName(runtimeExe) ?? AppContext.BaseDirectory;
			var stackKitsDirectory = Path.Combine(runtimeDirectory, "stackkits");
			if (Directory.Exists(stackKitsDirectory))
			{
				start.Environment["HOMELAB_STACKKITS_DIR"] = stackKitsDirectory;
				var stackKitCLI = Path.Combine(stackKitsDirectory, "bin", "stackkit.exe");
				if (File.Exists(stackKitCLI))
				{
					start.Environment["HOMELAB_STACKKIT_CLI"] = stackKitCLI;
				}
				var stackKitSpecTemplates = Path.Combine(stackKitsDirectory, "spec-templates");
				if (Directory.Exists(stackKitSpecTemplates))
				{
					start.Environment["HOMELAB_STACKKIT_SPEC_TEMPLATES"] = stackKitSpecTemplates;
				}
				var stackKitCompatibilityManifest = Path.Combine(stackKitsDirectory, "stackkits-compatibility-v1.json");
				if (File.Exists(stackKitCompatibilityManifest))
				{
					start.Environment["HOMELAB_STACKKIT_COMPATIBILITY_MANIFEST"] = stackKitCompatibilityManifest;
				}
			}
			start.Environment["HOMELAB_AGENT_BINARY_LINUX_AMD64"] = Path.Combine(runtimeDirectory, "homelab-linux-amd64");
			start.Environment["HOMELAB_STACKKIT_RELEASE_BUNDLE"] = Path.Combine(runtimeDirectory, "stackkit-release-linux-amd64.tar.gz");
			var guardReleaseDirectory = Path.Combine(runtimeDirectory, "guard-release");
			if (Directory.Exists(guardReleaseDirectory))
			{
				start.Environment["HOMELAB_GUARD_RELEASE_DIR"] = guardReleaseDirectory;
				start.Environment["HOMELAB_GUARD_RELEASE_SOURCE"] = "kombifyio/Homelab";
				start.Environment["HOMELAB_STACKKIT_RELEASE_BUNDLE"] = Path.Combine(guardReleaseDirectory, "stackkit-runtime-linux-amd64.tar.gz");
				start.Environment["HOMELAB_AGENT_BINARY_LINUX_AMD64"] = Path.Combine(guardReleaseDirectory, "homelab-linux-amd64");
			}

            _runtimeProcess = new SupervisedProcess();
            _runtimeProcess.Start(start, line => AppendRuntimeLog(logPath, line), shutdownOnInputClose: true);
        }
        catch (Exception ex)
        {
            ShowLocalStartFailure($"homelab.exe could not be started: {ex.Message}", logPath);
            return false;
        }

        var timeout = TimeSpan.FromSeconds(Math.Clamp(_config.RuntimeStartupTimeoutSeconds, 3, 120));
        var deadline = DateTimeOffset.UtcNow.Add(timeout);

        while (DateTimeOffset.UtcNow < deadline)
        {
            await Task.Delay(TimeSpan.FromMilliseconds(500));

            if (_runtimeProcess is { HasExited: true })
            {
                var exitCode = _runtimeProcess.ExitCode;
                StopStartedLocalRuntime();
                ShowLocalStartFailure(LocalRuntimeFailureDetail(logPath, exitCode), logPath);
                return false;
            }

            if (await LocalRuntimeReadyAsync(TimeSpan.FromSeconds(2)))
            {
                AppendRuntimeLog(logPath, $"[{DateTimeOffset.Now:u}] local runtime ready at {_config.LocalOrigin()}");
                return true;
            }
        }

        StopStartedLocalRuntime();
        ShowLocalStartFailure(
            $"The local runtime did not answer at {_config.LocalOrigin()} within {timeout.TotalSeconds:N0} seconds.",
            logPath);
        return false;
    }

    private async Task<bool> LocalRuntimeReadyAsync(TimeSpan timeout)
    {
        var uri = LocalAuthModeUri();
        if (uri is null)
        {
            return false;
        }

        using var timeoutCts = new CancellationTokenSource(timeout);
        try
        {
            using var request = new HttpRequestMessage(HttpMethod.Get, uri);
            using var response = await _httpClient.SendAsync(request, timeoutCts.Token);
            return response.IsSuccessStatusCode;
        }
        catch
        {
            return false;
        }
    }

    private Uri? LocalAuthModeUri()
    {
        var local = _config.LocalUiUri();
        return local is null ? null : new Uri(local, "/api/v1/auth/mode");
    }

    private string RuntimeListenAddr()
    {
        var local = _config.LocalUiUri();
        return local is null ? "127.0.0.1:5260" : RuntimeAddr(local.Host, local.Port);
    }

    // The gRPC listener stays next to the HTTP listener: same host, port + 3
    // (default 5260 -> 5263), so a non-default local port remains conflict-free.
    private string RuntimeGrpcAddr()
    {
        var local = _config.LocalUiUri();
        return local is null ? "127.0.0.1:5263" : RuntimeAddr(local.Host, local.Port + 3);
    }

    private static string RuntimeAddr(string host, int port)
    {
        if (host.Equals("localhost", StringComparison.OrdinalIgnoreCase))
        {
            host = "127.0.0.1";
        }

        if (IPAddress.TryParse(host, out var ip) && ip.AddressFamily == AddressFamily.InterNetworkV6)
        {
            host = $"[{host}]";
        }

        return $"{host}:{port}";
    }

    private string ResolveRuntimeExecutablePath()
    {
        if (!string.IsNullOrWhiteSpace(_config.RuntimeExecutablePath) && File.Exists(_config.RuntimeExecutablePath))
        {
            return _config.RuntimeExecutablePath;
        }

        var bundled = Path.Combine(AppContext.BaseDirectory, "homelab.exe");
        return File.Exists(bundled) ? bundled : "";
    }

    private string EnsureRuntimeSessionSecret()
    {
        return EnsureLocalCredential(ClientConfig.CredentialTarget(LocalRuntimeSessionCredentialTarget));
    }

    private string EnsureLocalDeviceToken()
    {
        return EnsureLocalCredential(ClientConfig.CredentialTarget(LocalDeviceCredentialTarget));
    }

    // The runtime uses the key's 32 bytes directly for AES-256-GCM, so it is 24
    // random bytes in unpadded base64url. It encrypts retained local data: a
    // present but malformed key is never replaced, because a new key would make
    // every stored secret unreadable.
    private static string EnsureRuntimeEncryptionKey()
    {
        var target = ClientConfig.CredentialTarget(LocalRuntimeEncryptionKeyCredentialTarget);
        var existing = WindowsCredentialStore.Read(target)?.Trim();
        if (!string.IsNullOrEmpty(existing))
        {
            if (existing.Length == 32 && existing.All(c => char.IsAsciiLetterOrDigit(c) || c is '-' or '_'))
            {
                return existing;
            }
            throw new InvalidOperationException(
                "The local encryption key in Windows Credential Manager is invalid. Restore it or reset the local state.");
        }

        var key = Convert.ToBase64String(RandomNumberGenerator.GetBytes(24)).Replace('+', '-').Replace('/', '_');
        WindowsCredentialStore.Write(target, Environment.UserName, key);
        return key;
    }

    private string ReadLocalDeviceToken()
    {
        return WindowsCredentialStore.Read(ClientConfig.CredentialTarget(LocalDeviceCredentialTarget))?.Trim() ?? "";
    }

    private static string EnsureLocalCredential(string target)
    {
        var existing = WindowsCredentialStore.Read(target)?.Trim();
        if (!string.IsNullOrWhiteSpace(existing) && existing.Length >= 64)
        {
            return existing;
        }

        var secret = Convert.ToHexString(RandomNumberGenerator.GetBytes(32)).ToLowerInvariant();
        WindowsCredentialStore.Write(target, Environment.UserName, secret);
        return secret;
    }

    private async Task<bool> BootstrapLocalDeviceSessionAsync()
    {
        var authority = _config.LocalUiUri();
        var token = ReadLocalDeviceToken();
        if (authority is null || string.IsNullOrWhiteSpace(token))
        {
            return false;
        }

        try
        {
            using var proofHttp = new HttpClient(new HttpClientHandler
            {
                AllowAutoRedirect = false,
                UseCookies = false,
            }) { Timeout = TimeSpan.FromSeconds(5) };
            var cookies = await LocalDeviceSessionProof.AuthenticateAsync(proofHttp, authority, token);
            foreach (var cookie in cookies)
                ImportSetCookieHeader(authority, cookie);
            AppendRuntimeLog(_config.RuntimeLogPath(), $"[{DateTimeOffset.Now:u}] local device session restored");
            return true;
        }
        catch (Exception ex)
        {
            AppendRuntimeLog(_config.RuntimeLogPath(), $"[{DateTimeOffset.Now:u}] local device session failed: {ex.Message}");
            return false;
        }
    }

    private void ImportSetCookieHeader(Uri endpoint, string setCookie)
    {
        var parts = setCookie.Split(';', StringSplitOptions.RemoveEmptyEntries | StringSplitOptions.TrimEntries);
        if (parts.Length == 0)
        {
            return;
        }

        var equalsAt = parts[0].IndexOf('=');
        if (equalsAt <= 0)
        {
            return;
        }

        var name = parts[0][..equalsAt].Trim();
        var value = parts[0][(equalsAt + 1)..].Trim();
        var path = "/";
        var httpOnly = false;
        var secure = false;
        var sameSite = CoreWebView2CookieSameSiteKind.Lax;

        foreach (var attr in parts.Skip(1))
        {
            if (attr.Equals("HttpOnly", StringComparison.OrdinalIgnoreCase))
            {
                httpOnly = true;
                continue;
            }
            if (attr.Equals("Secure", StringComparison.OrdinalIgnoreCase))
            {
                secure = true;
                continue;
            }
            if (attr.StartsWith("Path=", StringComparison.OrdinalIgnoreCase))
            {
                path = attr["Path=".Length..].Trim();
                continue;
            }
            if (attr.Equals("SameSite=None", StringComparison.OrdinalIgnoreCase))
            {
                sameSite = CoreWebView2CookieSameSiteKind.None;
                continue;
            }
            if (attr.Equals("SameSite=Strict", StringComparison.OrdinalIgnoreCase))
            {
                sameSite = CoreWebView2CookieSameSiteKind.Strict;
            }
        }

        var cookie = _webView.CoreWebView2.CookieManager.CreateCookie(name, value, endpoint.Host, path);
        cookie.IsHttpOnly = httpOnly;
        cookie.IsSecure = secure;
        cookie.SameSite = sameSite;
        _webView.CoreWebView2.CookieManager.AddOrUpdateCookie(cookie);
    }

    private void StopStartedLocalRuntime()
    {
        _runtimeProcess?.Stop();
        _runtimeProcess = null;
    }

    private void OnNavigationStarting(object? sender, CoreWebView2NavigationStartingEventArgs args)
    {
        if (!Uri.TryCreate(args.Uri, UriKind.Absolute, out var uri))
        {
            return;
        }
        // NavigateToString uses about:blank for shell-owned progress/error pages.
        if (uri.AbsoluteUri == "about:blank") return;
#if CLOUD_DESKTOP
        if (uri.Scheme != Uri.UriSchemeHttps || !string.IsNullOrEmpty(uri.UserInfo))
        {
            args.Cancel = true;
            return;
        }
        if (IsCloudProductMode() && IsProductSignOutNavigation(uri))
        {
            // The person's own sign-out: the shell signs out this device (device sign-in §5).
            args.Cancel = true;
            _ = PersonSignOutAsync();
            return;
        }
        if (IsWindowsBrowserLoginHandoff(uri))
        {
            args.Cancel = true;
            _ = BeginCloudProductLoginAsync();
            return;
        }

        if (IsCloudDeviceLogin(uri))
        {
            args.Cancel = true;
            if (HasDeviceCode(uri))
            {
                OpenInBrowser(args.Uri);
                return;
            }

            _ = BeginCloudDeviceLoginAsync();
            return;
        }

        if (IsHostedAuthUri(uri))
        {
            args.Cancel = true;
            OpenInBrowser(args.Uri);
            ShowBrowserHandoff(args.Uri);
            return;
        }
#endif
        if (!ClientAuthority.AllowsProductNavigation(uri, _boundServerProfile?.BaseUrl ?? _config.InitialUrl()))
            args.Cancel = true;
    }

#if CLOUD_DESKTOP
    private async Task BeginCloudDeviceLoginAsync()
    {
        _cloudLoginCancellation?.Cancel();
        _cloudLoginCancellation = new CancellationTokenSource();
        var token = _cloudLoginCancellation.Token;

        NavigateHtml(RenderDeviceLoginStarting());

        try
        {
            var code = await RequestDeviceCodeAsync(token);
            if (string.IsNullOrWhiteSpace(code.DeviceCode) || string.IsNullOrWhiteSpace(code.UserCode))
            {
                throw new InvalidOperationException("kombify Cloud did not return a complete device code response.");
            }

            var verificationUrl = BuildVerificationUrl(code);
            NavigateHtml(RenderDeviceLoginWaiting(code, verificationUrl));
            OpenInBrowser(verificationUrl);
            await PollCloudAuthorizationAsync(code, token);
        }
        catch (OperationCanceledException)
        {
            // A new cloud-login attempt replaced this one.
        }
        catch (Exception ex)
        {
            ShowSignInFailure($"The client could not create a kombify device login. {ex.Message}");
        }
    }

    private async Task<CloudDeviceCodeResponse> RequestDeviceCodeAsync(CancellationToken token)
    {
        using var payload = JsonContent(new { tool = _config.ToolName });
        using var response = await _httpClient.PostAsync(_config.CloudDeviceCodeEndpoint, payload, token);
        var body = await response.Content.ReadAsStringAsync(token);

        if (!response.IsSuccessStatusCode)
        {
            throw new InvalidOperationException(CloudError(response.StatusCode, body));
        }

        return JsonSerializer.Deserialize<CloudDeviceCodeResponse>(body, JsonOptions)
            ?? throw new InvalidOperationException("kombify Cloud returned an empty device code response.");
    }

    private async Task PollCloudAuthorizationAsync(CloudDeviceCodeResponse code, CancellationToken token)
    {
        var interval = TimeSpan.FromSeconds(Math.Clamp(code.Interval, 3, 30));
        var timeout = DateTimeOffset.UtcNow.AddSeconds(Math.Clamp(code.ExpiresIn, 60, 1800));

        while (DateTimeOffset.UtcNow < timeout)
        {
            await Task.Delay(interval, token);

            using var payload = JsonContent(new { device_code = code.DeviceCode });
            using var response = await _httpClient.PostAsync(_config.CloudDevicePollEndpoint, payload, token);
            var body = await response.Content.ReadAsStringAsync(token);

            if (response.IsSuccessStatusCode)
            {
                SaveCloudSession(body);
                NavigateHtml(RenderDeviceLoginSuccess(_config.CloudUiUrl));
                return;
            }

            if (response.StatusCode == HttpStatusCode.Accepted
                || body.Contains("authorization_pending", StringComparison.OrdinalIgnoreCase))
            {
                continue;
            }

            if ((int)response.StatusCode == 429 || body.Contains("slow_down", StringComparison.OrdinalIgnoreCase))
            {
                interval = interval.Add(TimeSpan.FromSeconds(2));
                continue;
            }

            if (response.StatusCode == HttpStatusCode.Gone
                || body.Contains("expired_token", StringComparison.OrdinalIgnoreCase))
            {
                ShowSignInFailure("The device code expired before it was approved.");
                return;
            }

            ShowSignInFailure(CloudError(response.StatusCode, body));
            return;
        }

        ShowSignInFailure("The device code expired before it was approved.");
    }

    private void ShowSignInFailure(string detail)
    {
        ShowFailure(ShellText.Get("SignInFailedTitle"), detail, "", () => _ = BeginCloudDeviceLoginAsync());
    }

    private static StringContent JsonContent<T>(T value)
    {
        return new StringContent(
            JsonSerializer.Serialize(value, JsonOptions),
            Encoding.UTF8,
            "application/json");
    }

    private static string BuildVerificationUrl(CloudDeviceCodeResponse code)
    {
        if (!string.IsNullOrWhiteSpace(code.VerificationUriComplete)
            && Uri.TryCreate(code.VerificationUriComplete, UriKind.Absolute, out _))
        {
            return code.VerificationUriComplete;
        }

        var baseUri = string.IsNullOrWhiteSpace(code.VerificationUri)
            ? "https://kombify.io/device"
            : code.VerificationUri;
        var separator = baseUri.Contains('?') ? '&' : '?';
        return $"{baseUri}{separator}code={Uri.EscapeDataString(code.UserCode)}";
    }

    private static bool IsCloudDeviceLogin(Uri uri)
    {
        return (uri.Host.Equals("app.kombify.io", StringComparison.OrdinalIgnoreCase)
                || uri.Host.Equals("kombify.io", StringComparison.OrdinalIgnoreCase))
            && (uri.AbsolutePath.Equals("/device", StringComparison.OrdinalIgnoreCase)
                || uri.AbsolutePath.StartsWith("/signin/start/", StringComparison.OrdinalIgnoreCase));
    }

    private static bool IsHostedAuthUri(Uri uri)
    {
        return uri.Host.Equals("app.kombify.io", StringComparison.OrdinalIgnoreCase)
            || uri.Host.Equals("kombify.io", StringComparison.OrdinalIgnoreCase)
            || uri.Host.Equals("login.kombify.io", StringComparison.OrdinalIgnoreCase);
    }

    private static bool IsWindowsBrowserLoginHandoff(Uri uri)
    {
        return ClientAuthority.AllowsProductNavigation(uri, ClientAuthority.CloudOrigin)
            && uri.Query.Contains("client=windows", StringComparison.OrdinalIgnoreCase)
            && uri.Query.Contains("open_browser=1", StringComparison.OrdinalIgnoreCase);
    }

    private static bool HasDeviceCode(Uri uri)
    {
        return uri.Query.Contains("code=", StringComparison.OrdinalIgnoreCase);
    }

    private static bool IsHttp(string value)
    {
        return Uri.TryCreate(value, UriKind.Absolute, out var uri)
            && (uri.Scheme == Uri.UriSchemeHttp || uri.Scheme == Uri.UriSchemeHttps);
    }

    private static void OpenInBrowser(string url)
    {
        Process.Start(new ProcessStartInfo { FileName = url, UseShellExecute = true });
    }

    private void ShowBrowserHandoff(string browserUrl)
    {
        NavigateHtml(RenderProgress(CloudLoginText.Get("WaitingTitle"), BrowserAgainHtml(browserUrl)));
    }

    // The sign-in page opened in the system browser; the link opens it there once more.
    private static string BrowserAgainHtml(string browserUrl) =>
        IsHttp(browserUrl)
            ? $"""<p class="again"><a href="{Html(browserUrl)}">{Html(ShellText.Get("OpenBrowserAgain"))}</a></p>"""
            : "";

    private void SaveCloudSession(string body)
    {
        var response = JsonSerializer.Deserialize<CloudAuthorizationResponse>(body, JsonOptions)
            ?? throw new InvalidOperationException("kombify Cloud returned an empty authorization response.");
        if (string.IsNullOrWhiteSpace(response.Token.AccessToken)
            || string.IsNullOrWhiteSpace(response.Token.RefreshToken)
            || string.IsNullOrWhiteSpace(response.Token.UserId))
        {
            throw new InvalidOperationException("kombify Cloud returned an incomplete authorization response.");
        }

        var accessTarget = CloudCredentialTarget("access-token");
        var refreshTarget = CloudCredentialTarget("refresh-token");
        WindowsCredentialStore.Write(accessTarget, response.Token.UserId, response.Token.AccessToken);
        try
        {
            WindowsCredentialStore.Write(refreshTarget, response.Token.UserId, response.Token.RefreshToken);
        }
        catch
        {
            WindowsCredentialStore.Delete(accessTarget);
            throw;
        }

        Directory.CreateDirectory(ClientConfig.StateDirectory());
        var path = Path.Combine(ClientConfig.StateDirectory(), "cloud-session.json");
        var envelope = new
        {
            connectedAt = DateTimeOffset.UtcNow,
            source = "kombify-cloud-device-code",
            credentialStore = "windows-credential-manager",
            accessCredentialTarget = accessTarget,
            refreshCredentialTarget = refreshTarget,
            expiresAt = response.Token.ExpiresAt,
            userId = response.Token.UserId,
            entitlement = response.Entitlements is null
                ? null
                : new { tool = response.Entitlements.Tool, expiresAt = response.Entitlements.ExpiresAt },
        };

        File.WriteAllText(path, JsonSerializer.Serialize(envelope, new JsonSerializerOptions
        {
            WriteIndented = true,
        }));
    }

    private string CloudCredentialTarget(string credentialKind)
    {
        var tool = new string(_config.ToolName
            .ToLowerInvariant()
            .Where(character => char.IsLetterOrDigit(character) || character is '-' or '_')
            .ToArray());
        if (string.IsNullOrWhiteSpace(tool))
        {
            tool = "stack";
        }

        return ClientConfig.CredentialTarget($"kombify/techstack/cloud/{DesktopEdition.Channel}/{tool}/{credentialKind}");
    }
#endif

    private void NavigateHtml(string html)
    {
        if (InvokeRequired)
        {
            BeginInvoke(new Action(() => NavigateHtml(html)));
            return;
        }

        _webView.CoreWebView2.NavigateToString(html);
    }

#if CLOUD_DESKTOP
    private static string CloudError(HttpStatusCode statusCode, string body)
    {
        var message = ExtractJsonError(body);
        return string.IsNullOrWhiteSpace(message)
            ? $"kombify Cloud returned {(int)statusCode} {statusCode}."
            : $"kombify Cloud returned {(int)statusCode} {statusCode}: {message}";
    }

    private static string ExtractJsonError(string body)
    {
        if (string.IsNullOrWhiteSpace(body))
        {
            return "";
        }

        try
        {
            using var doc = JsonDocument.Parse(body);
            var root = doc.RootElement;
            foreach (var key in new[] { "error_description", "message", "error" })
            {
                if (root.TryGetProperty(key, out var value) && value.ValueKind == JsonValueKind.String)
                {
                    return value.GetString() ?? "";
                }
            }
        }
        catch
        {
            return body.Length > 240 ? body[..240] : body;
        }

        return "";
    }

    private static string RenderDeviceLoginStarting()
    {
        return RenderProgress(ShellText.Get("SigningInTitle"));
    }

    private static string RenderDeviceLoginWaiting(CloudDeviceCodeResponse code, string verificationUrl)
    {
        return RenderProgress(CloudLoginText.Get("WaitingTitle"),
            $"""<div class="code">{Html(code.UserCode)}</div>{BrowserAgainHtml(verificationUrl)}""");
    }

    private static string RenderDeviceLoginSuccess(string cloudUiUrl)
    {
        return RenderShell(ShellText.Get("SignedInTitle"), spinner: false,
            IsHttp(cloudUiUrl)
                ? $"""<div class="actions"><a class="primary" href="{Html(cloudUiUrl)}">{Html(ShellText.Get("OpenHomelab"))}</a></div>"""
                : "");
    }
#endif

    // ── The shell's own pages (D-2026-11-17-03, concept "when something does not work") ──────────────────
    // Before the product page exists, the shell draws three states itself: something is starting (one line
    // and a spinner), something failed (one sentence, "Try again", "Get help", the technical reason behind
    // "What happened"), and the browser hand-off of a sign-in. Light and dark follow Windows; the words follow
    // the Windows display language (ShellMessages.*.resx). Nothing here names a component of the runtime.

    private static string RenderProgress(string title, string extraHtml = "") =>
        RenderShell(title, spinner: true, extraHtml);

    private static string RenderFailure(string title, string technicalDetail, string technicalHtml)
    {
        var actions = $"""
<div class="actions">
<button type="button" class="primary" onclick="shell('retry')">{Html(ShellText.Get("TryAgain"))}</button>
<button type="button" onclick="shell('help')">{Html(ShellText.Get("GetHelp"))}</button>
</div>
<details><summary>{Html(ShellText.Get("WhatHappened"))}</summary><p>{Html(technicalDetail)}</p>{technicalHtml}</details>
""";
        return RenderShell(title, spinner: false, actions, failed: true);
    }

    private static string RenderShell(string title, bool spinner, string extraHtml, bool failed = false)
    {
        var logo = LogoDataUri();
        var lang = Html(System.Globalization.CultureInfo.CurrentUICulture.Name);
        var dir = System.Globalization.CultureInfo.CurrentUICulture.TextInfo.IsRightToLeft ? "rtl" : "ltr";
        var mark = logo.Length == 0 ? "" : $"""<div class="mark"><img src="{logo}" alt=""></div>""";
        var progress = spinner ? """<div class="spinner" role="progressbar" aria-busy="true"></div>""" : "";
        return $$"""
<!doctype html>
<html lang="{{lang}}" dir="{{dir}}"><head><meta charset="utf-8"><meta name="color-scheme" content="light dark"><title>{{Html(ShellText.ProductName)}}</title>
<style>
:root{--background:oklch(0.9749 0.0025 228.8);--foreground:oklch(0.1572 0.0039 229.1);--card:oklch(1 0 0);--muted-foreground:oklch(0.4572 0.0124 248.1);--border:oklch(0.1572 0.0039 229.1 / 14%);--primary:oklch(0.30 0.09 255);--primary-foreground:oklch(0.985 0 0)}
@media (prefers-color-scheme:dark){:root{--background:oklch(0.1572 0.0039 229.1);--foreground:oklch(0.9674 0.0013 286.4);--card:oklch(0.2071 0.0076 248.2);--muted-foreground:oklch(0.7118 0.0129 286.1);--border:oklch(0.9674 0.0013 286.4 / 14%);--primary:oklch(0.55 0.14 255)} }
html,body{height:100%;margin:0}body{display:grid;place-items:center;background:var(--background);color:var(--foreground);font-family:"Segoe UI Variable Text","Segoe UI",system-ui,sans-serif}
main{width:min(520px,calc(100vw - 32px));display:flex;flex-direction:column;align-items:center;text-align:center;gap:20px}
.mark{width:64px;height:64px;border-radius:16px;background:var(--card);border:1px solid var(--border);display:grid;place-items:center}.mark img{width:40px;height:40px;object-fit:contain}
h1{margin:0;font-size:22px;line-height:1.3;font-weight:600}
.actions{display:flex;flex-wrap:wrap;justify-content:center;gap:10px}
button,.actions a{font:inherit;font-size:15px;font-weight:600;padding:10px 20px;border-radius:999px;border:1px solid var(--border);background:var(--card);color:var(--foreground);cursor:pointer;text-decoration:none}
button.primary,.actions a.primary{background:var(--primary);border-color:var(--primary);color:var(--primary-foreground)}
button:focus-visible,a:focus-visible,summary:focus-visible{outline:2px solid var(--primary);outline-offset:2px}
details{width:100%;color:var(--muted-foreground);font-size:13px}summary{cursor:pointer;list-style-position:inside}
details p{margin:12px 0 0;text-align:start;line-height:1.5}
pre{max-height:220px;overflow:auto;text-align:start;background:var(--card);border:1px solid var(--border);color:var(--foreground);padding:12px;border-radius:8px;font-size:12px;line-height:1.45;white-space:pre-wrap}
.path{font-family:Consolas,monospace;word-break:break-all}
.code{padding:12px 18px;border:1px solid var(--border);border-radius:12px;background:var(--card);font-family:Consolas,monospace;font-size:28px;font-weight:700;letter-spacing:.12em}
.again a{color:var(--muted-foreground);font-size:14px}
.spinner{width:28px;height:28px;border-radius:50%;border:3px solid var(--border);border-top-color:var(--primary);animation:spin 1s linear infinite}
@keyframes spin{to{transform:rotate(360deg)} }@media (prefers-reduced-motion:reduce){.spinner{animation-duration:3s} }
</style></head><body><main{{(failed ? " role=\"alert\"" : "")}}>{{mark}}<h1>{{Html(title)}}</h1>{{progress}}{{extraHtml}}</main>
<script>function shell(action){try{chrome.webview.postMessage({type:"kombify.homelab.shell."+action+".v1"});}catch(e){ } }</script>
</body></html>
""";
    }

    private static string LocalRuntimeFailureDetail(string logPath, int exitCode)
    {
        var tail = ClientDiagnostics.ReadTail(logPath);
        if (tail.Contains("start embedded postgres", StringComparison.OrdinalIgnoreCase))
        {
            return "The bundled local Postgres database failed to start. Check postgres.log in the runtime data "
                + "directory; a stale postmaster.pid after a crash or an antivirus block are the most common causes.";
        }

        if (tail.Contains("Only one usage of each socket address", StringComparison.OrdinalIgnoreCase)
            || tail.Contains("address already in use", StringComparison.OrdinalIgnoreCase))
        {
            return "The local runtime could not bind its address (default 127.0.0.1:5260) because another process "
                + "is already using it. Close the conflicting process or configure a different local UI URL.";
        }

        if (tail.Contains("DATABASE_URL", StringComparison.OrdinalIgnoreCase)
            || tail.Contains("control-plane database is required", StringComparison.OrdinalIgnoreCase))
        {
            return "The local runtime could not use its control-plane database. A DATABASE_URL override points at "
                + "an unreachable Postgres or the embedded database was disabled; remove the override to use the "
                + "bundled local database.";
        }

        return $"The local runtime exited with code {exitCode}.";
    }

    private static string RuntimeLogExtraHtml(string logPath)
    {
        var tail = ClientDiagnostics.ReadTail(logPath);
        var pre = string.IsNullOrWhiteSpace(tail) ? "" : $"""<pre>{Html(tail)}</pre>""";
        return $"""<p class="path">{Html(logPath)}</p>{pre}""";
    }

    internal static void AppendRuntimeLog(string path, string? line)
    {
        if (string.IsNullOrWhiteSpace(line))
        {
            return;
        }

        try
        {
            lock (RuntimeLogLock)
            {
                File.AppendAllText(path, line + Environment.NewLine, Encoding.UTF8);
            }
        }
        catch
        {
            // Runtime logging must never crash the shell.
        }
    }

    private static Icon? LoadIcon()
    {
        var path = Path.Combine(AppContext.BaseDirectory, "Assets", "kombify-navy.ico");
        return File.Exists(path) ? new Icon(path) : null;
    }

    private static string LogoDataUri()
    {
        var path = Path.Combine(AppContext.BaseDirectory, "Assets", "kombify-navy.png");
        return File.Exists(path)
            ? "data:image/png;base64," + Convert.ToBase64String(File.ReadAllBytes(path))
            : "";
    }

    private static string Html(string value) => System.Net.WebUtility.HtmlEncode(value);
}
