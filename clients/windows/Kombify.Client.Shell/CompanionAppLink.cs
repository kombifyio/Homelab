using System.Diagnostics.CodeAnalysis;
using System.IO.Pipes;
using System.Runtime.InteropServices;
using System.Security.Principal;
using System.Text;
using System.Text.Json;
using System.Text.RegularExpressions;
using Microsoft.Win32;
using Microsoft.Win32.SafeHandles;

namespace Kombify.Client.Shell;

// Native client of Companion AppLink v1 (workspace
// platform/standards/companion-applink.v1.md and its JSON schema; Go reference
// kombify-AI-Platform speechkit/app/host/applink). Each request is one verified
// connection to the per-user/session pipe; no token, account or credential is
// ever sent or received. The shell never downloads the Companion; the only
// install it runs is CompanionInstaller's, of the installer a client carries.

/// <summary>Installed, running and enabled are independent; enabled is local device state only.</summary>
public sealed record CompanionStatus(bool Installed, bool Running, bool Enabled);

/// <summary>A named AppLink failure. The message is the code; no OS error, path or value crosses it.</summary>
public sealed class CompanionAppLinkException(string code) : Exception(code)
{
    public string Code { get; } = code;
}

/// <summary>Wire and local error codes of the contract ("Errors and versioning").</summary>
public static class CompanionAppLinkError
{
    public const string InvalidRequest = "invalid_request";
    public const string UnsupportedVersion = "unsupported_version";
    public const string UnsupportedOperation = "unsupported_operation";
    public const string Disabled = "disabled";
    public const string NotForeground = "not_foreground";
    public const string RateLimited = "rate_limited";
    public const string PeerDenied = "peer_denied";
    public const string HostUnavailable = "host_unavailable";
    public const string NotInstalled = "not_installed";
    public const string InvalidInstallation = "invalid_installation";
    public const string InvalidPreference = "invalid_preference";
    public const string StartFailed = "start_failed";
    public const string StartTimeout = "start_timeout";
    public const string AppNotRegistered = "app_not_registered";
    public const string SchemeNotRegistered = "scheme_not_registered";
    public const string OpenFailed = "open_failed";
    public const string InvalidResponse = "invalid_response";
    public const string ElevatedPeerRefused = "elevated_peer_refused";
    public const string EndpointConflict = "endpoint_conflict";
    public const string TransportUnavailable = "transport_unavailable";
    public const string NotRunning = "not_running";

    internal static readonly HashSet<string> Wire = new(StringComparer.Ordinal)
    {
        InvalidRequest, UnsupportedVersion, UnsupportedOperation, Disabled,
        NotForeground, HostUnavailable, RateLimited, PeerDenied,
    };
}

/// <summary>The products a context report or an open-in-app target may name (a claim, not OS evidence).</summary>
public static class CompanionProduct
{
    public const string Techstack = "techstack";
    public const string Paperwork = "paperwork";
    public const string Workbench = "workbench";

    internal static bool Valid(string? product) => product is Techstack or Paperwork or Workbench;
}

/// <summary>
/// Metadata-only subset of SurfaceContextV2 admitted by AppLink (version 2).
/// Selection, drafts, forms, graphs, account choice and credentials have no field.
/// Empty strings mean absent and are omitted on the wire.
/// </summary>
public sealed record CompanionContext
{
    public required string Surface { get; init; }
    public required CompanionScope Scope { get; init; }
    public string? Route { get; init; }
    public string? Locale { get; init; }
    public CompanionPage? Page { get; init; }
}

/// <summary>Scope kinds: none, project, server, paperwork (with module), office (with officeId).</summary>
public sealed record CompanionScope
{
    public required string Kind { get; init; }
    public string? ProjectId { get; init; }
    public string? ServerId { get; init; }
    public string? EnvironmentId { get; init; }
    public string? Module { get; init; }
    public string? ResourceType { get; init; }
    public string? ResourceId { get; init; }
    public string? OfficeId { get; init; }

    public static CompanionScope None { get; } = new() { Kind = "none" };
}

/// <summary>Page metadata: at most 16 facts of short codes, never free text.</summary>
public sealed record CompanionPage
{
    public string? Tool { get; init; }
    public string? State { get; init; }
    public IReadOnlyDictionary<string, string>? Facts { get; init; }
}

/// <summary>
/// The app side of Companion AppLink v1 on Windows: discovery, the shared device
/// switch (starting the registered Companion in the background on enable) and
/// foreground context reports. Every call refuses an elevated, low-integrity or
/// AppContainer caller before it reads or changes anything, and verifies the
/// pipe server (same user and session, medium integrity, registered executable)
/// before writing a byte to it.
/// </summary>
public static partial class CompanionAppLink
{
    public const int Version = 1;
    public const int MaxFrameBytes = 8192;
    public const string RegistrationKey = @"Software\kombify\CompanionDesktop";
    public const string BackgroundArgument = "--companion-background";

    private const int MaxDepth = 12;
    private static readonly TimeSpan RequestDeadline = TimeSpan.FromSeconds(2);
    private static readonly TimeSpan StartWait = TimeSpan.FromSeconds(5);
    private static readonly TimeSpan StartPoll = TimeSpan.FromMilliseconds(100);

    /// <summary>
    /// True when this process may use the interface at all (medium integrity, not
    /// AppContainer). A product shows its Companion switch only then; an elevated
    /// shell leaves the switch to its medium shell or a normal Companion launch.
    /// </summary>
    public static bool IsCallerEligible()
    {
        try
        {
            return Eligibility(LocalPeer()) is null;
        }
        catch (Exception error) when (error is not OutOfMemoryException)
        {
            // Any failure to inspect this process's own token means "not eligible", never an
            // exception into a caller that asks once and caches the answer.
            return false;
        }
    }

    /// <summary>
    /// Reads installation, listener and switch without starting anything. A
    /// missing installation is a status (all false), not an error; access, busy,
    /// version and peer failures stay errors.
    /// </summary>
    public static async Task<CompanionStatus> DiscoverAsync(CancellationToken cancellationToken = default)
    {
        RequireEligibleCaller();
        var path = Registration();
        if (path is null) return new CompanionStatus(false, false, false);
        var enabled = EnabledPreference();
        try
        {
            var response = await CallAsync(StatusRequest(), "status", cancellationToken).ConfigureAwait(false);
            return response.Status ?? throw Fail(CompanionAppLinkError.InvalidResponse);
        }
        catch (CompanionAppLinkException error) when (error.Code == CompanionAppLinkError.NotRunning)
        {
            // While stopped, enabled is the device preference.
            return new CompanionStatus(true, false, enabled);
        }
    }

    /// <summary>
    /// Switches the shared device setting. A running host applies it; while stopped
    /// the preference is written and, on enable only, the registered executable is
    /// started directly with <see cref="BackgroundArgument"/> and awaited for at most
    /// five seconds. Not installed is <c>not_installed</c>, never a download.
    /// </summary>
    public static async Task<CompanionStatus> SetEnabledAsync(bool enabled, CancellationToken cancellationToken = default)
    {
        CompanionStatus state;
        try
        {
            state = await DiscoverAsync(cancellationToken).ConfigureAwait(false);
        }
        catch (CompanionAppLinkException error) when (error.Code == CompanionAppLinkError.HostUnavailable)
        {
            // A running host whose reconciliation failed stays reachable for an
            // explicit switch; never treat it as stopped or start a second one.
            return await SwitchAsync(enabled, cancellationToken).ConfigureAwait(false);
        }
        if (!state.Installed) throw Fail(CompanionAppLinkError.NotInstalled);
        if (state.Running) return await SwitchAsync(enabled, cancellationToken).ConfigureAwait(false);

        SetPreference(enabled);
        state = state with { Enabled = enabled };
        if (!enabled) return state;

        StartInstalled(Registration() ?? throw Fail(CompanionAppLinkError.NotInstalled));
        using var wait = CancellationTokenSource.CreateLinkedTokenSource(cancellationToken);
        wait.CancelAfter(StartWait);
        try
        {
            while (true)
            {
                await Task.Delay(StartPoll, wait.Token).ConfigureAwait(false);
                state = await DiscoverAsync(wait.Token).ConfigureAwait(false);
                if (state.Running) return state;
            }
        }
        catch (OperationCanceledException) when (!cancellationToken.IsCancellationRequested)
        {
            // The chosen preference stays; a failed start never claims running.
            throw Fail(CompanionAppLinkError.StartTimeout);
        }
        catch (CompanionAppLinkException error) when (error.Code == CompanionAppLinkError.TransportUnavailable
            && wait.IsCancellationRequested && !cancellationToken.IsCancellationRequested)
        {
            throw Fail(CompanionAppLinkError.StartTimeout);
        }
    }

    /// <summary>
    /// Sends one foreground metadata report. The calling process must own the
    /// foreground top-level window; the host admits one report per peer per two
    /// seconds (<c>rate_limited</c>) and expires it after five.
    /// </summary>
    public static async Task ReportContextAsync(string product, CompanionContext context, CancellationToken cancellationToken = default)
    {
        RequireEligibleCaller();
        if (!CompanionProduct.Valid(product) || !ValidContext(context)) throw Fail(CompanionAppLinkError.InvalidRequest);
        var response = await CallAsync(ContextRequest(product, context), "context", cancellationToken).ConfigureAwait(false);
        if (response.Accepted != true) throw Fail(CompanionAppLinkError.InvalidResponse);
    }

    /// <summary>True when a top-level window of this process is the foreground window.</summary>
    public static bool OwnsForegroundWindow()
    {
        var window = AppLinkNative.GetForegroundWindow();
        if (window == IntPtr.Zero) return false;
        _ = AppLinkNative.GetWindowThreadProcessId(window, out var pid);
        return pid == (uint)Environment.ProcessId;
    }

    private static async Task<CompanionStatus> SwitchAsync(bool enabled, CancellationToken cancellationToken)
    {
        var response = await CallAsync(SwitchRequest(enabled), "switch", cancellationToken).ConfigureAwait(false);
        return response.Status ?? throw Fail(CompanionAppLinkError.InvalidResponse);
    }

    internal static CompanionAppLinkException Fail(string code) => new(code);

    private static void RequireEligibleCaller()
    {
        if (Eligibility(LocalPeer()) is { } code) throw Fail(code);
    }

    private static string? Eligibility(Peer peer) =>
        peer.Integrity >= 0x3000 ? CompanionAppLinkError.ElevatedPeerRefused
        : peer.Integrity < 0x2000 || peer.AppContainer ? CompanionAppLinkError.PeerDenied
        : null;

    // ── Transport: one verified connection per request ────────────────────

    private static async Task<WireResponse> CallAsync(byte[] frame, string id, CancellationToken cancellationToken)
    {
        if (frame.Length + 1 > MaxFrameBytes) throw Fail(CompanionAppLinkError.InvalidRequest);
        using var deadline = CancellationTokenSource.CreateLinkedTokenSource(cancellationToken);
        deadline.CancelAfter(RequestDeadline);
        using var pipe = await DialAsync(deadline.Token).ConfigureAwait(false);
        byte[] line;
        try
        {
            var request = new byte[frame.Length + 1];
            frame.CopyTo(request, 0);
            request[^1] = (byte)'\n';
            await pipe.WriteAsync(request, deadline.Token).ConfigureAwait(false);
            await pipe.FlushAsync(deadline.Token).ConfigureAwait(false);
            line = await ReadFrameAsync(pipe, deadline.Token).ConfigureAwait(false);
        }
        catch (OperationCanceledException) when (!cancellationToken.IsCancellationRequested)
        {
            throw Fail(CompanionAppLinkError.TransportUnavailable);
        }
        catch (IOException)
        {
            // Close before a valid response is a transport failure, never success or off.
            throw Fail(CompanionAppLinkError.TransportUnavailable);
        }
        catch (ObjectDisposedException)
        {
            throw Fail(CompanionAppLinkError.TransportUnavailable);
        }
        var response = ParseResponse(line);
        if (response.Error == CompanionAppLinkError.PeerDenied && response.Id.Length == 0)
            throw Fail(CompanionAppLinkError.PeerDenied); // the host's fixed pre-request refusal
        if (!string.Equals(response.Id, id, StringComparison.Ordinal)) throw Fail(CompanionAppLinkError.InvalidResponse);
        if (response.Error is { } error) throw Fail(error);
        return response;
    }

    private static async Task<byte[]> ReadFrameAsync(Stream pipe, CancellationToken cancellationToken)
    {
        var buffer = new byte[MaxFrameBytes];
        var length = 0;
        while (length < buffer.Length)
        {
            var read = await pipe.ReadAsync(buffer.AsMemory(length), cancellationToken).ConfigureAwait(false);
            if (read == 0) throw Fail(CompanionAppLinkError.TransportUnavailable);
            var end = Array.IndexOf(buffer, (byte)'\n', length, read);
            length += read;
            if (end >= 0)
            {
                // One response followed by LF, then close: anything after it is malformed.
                if (end != length - 1) throw Fail(CompanionAppLinkError.InvalidResponse);
                return buffer.AsSpan(0, end).ToArray();
            }
        }
        throw Fail(CompanionAppLinkError.InvalidResponse); // oversize frame
    }

    private static async Task<NamedPipeClientStream> DialAsync(CancellationToken cancellationToken)
    {
        var me = LocalPeer();
        if (Eligibility(me) is { } refused) throw Fail(refused);
        var name = Endpoint(me);
        SafePipeHandle handle;
        while (true)
        {
            // Identification level: a squatting server can never impersonate this client.
            handle = AppLinkNative.CreateFile(name, AppLinkNative.GenericRead | AppLinkNative.GenericWrite, 0, IntPtr.Zero,
                AppLinkNative.OpenExisting,
                AppLinkNative.FileFlagOverlapped | AppLinkNative.SecuritySqosPresent | AppLinkNative.SecurityIdentification,
                IntPtr.Zero);
            if (!handle.IsInvalid) break;
            var error = Marshal.GetLastPInvokeError();
            handle.Dispose();
            switch (error)
            {
                case AppLinkNative.ErrorFileNotFound or AppLinkNative.ErrorPathNotFound:
                    throw Fail(CompanionAppLinkError.NotRunning); // the only meaning of stopped
                case AppLinkNative.ErrorAccessDenied:
                    throw Fail(CompanionAppLinkError.EndpointConflict);
                case AppLinkNative.ErrorPipeBusy:
                    // Bounded by the request deadline; never a busy loop.
                    try { await Task.Delay(50, cancellationToken).ConfigureAwait(false); }
                    catch (OperationCanceledException) { throw Fail(CompanionAppLinkError.TransportUnavailable); }
                    continue;
                default:
                    throw Fail(CompanionAppLinkError.TransportUnavailable);
            }
        }
        NamedPipeClientStream? pipe = null;
        try
        {
            if (!AppLinkNative.GetNamedPipeServerProcessId(handle, out var serverPid))
                throw Fail(CompanionAppLinkError.PeerDenied);
            var server = ProcessPeer(serverPid);
            var registered = Registration();
            if (server.Sid != me.Sid || server.Session != me.Session || Eligibility(server) is not null
                || registered is null || !string.Equals(server.Executable, registered, StringComparison.OrdinalIgnoreCase))
                throw Fail(CompanionAppLinkError.PeerDenied); // no application bytes go to a foreign server
            pipe = new NamedPipeClientStream(PipeDirection.InOut, isAsync: true, isConnected: true, handle);
            return pipe;
        }
        catch (Exception error) when (error is not CompanionAppLinkException)
        {
            pipe?.Dispose();
            handle.Dispose();
            throw Fail(error is IOException ? CompanionAppLinkError.TransportUnavailable : CompanionAppLinkError.PeerDenied);
        }
        catch
        {
            pipe?.Dispose();
            handle.Dispose();
            throw;
        }
    }

    private static string Endpoint(Peer peer) =>
        $@"\\.\pipe\kombify.companion.applink.v1.{peer.Sid}.{peer.Session}";

    // ── OS evidence (never read from a message) ────────────────────────────

    private readonly record struct Peer(uint Pid, uint Session, string Sid, uint Integrity, bool AppContainer, string Executable);

    private static Peer LocalPeer() => ProcessPeer((uint)Environment.ProcessId);

    private static Peer ProcessPeer(uint pid)
    {
        using var process = AppLinkNative.OpenProcess(AppLinkNative.ProcessQueryLimitedInformation, false, pid);
        if (process.IsInvalid) throw Fail(CompanionAppLinkError.PeerDenied);
        if (!AppLinkNative.OpenProcessToken(process, AppLinkNative.TokenQuery, out var token) || token.IsInvalid)
        {
            token.Dispose();
            throw Fail(CompanionAppLinkError.PeerDenied);
        }
        using (token)
        {
            var sid = TokenSid(token, AppLinkNative.TokenUser);
            var session = TokenUInt32(token, AppLinkNative.TokenSessionId);
            var integrity = TokenSid(token, AppLinkNative.TokenIntegrityLevel);
            var container = TokenUInt32(token, AppLinkNative.TokenIsAppContainer);
            var lastAuthority = integrity.Value[(integrity.Value.LastIndexOf('-') + 1)..];
            if (!uint.TryParse(lastAuthority, System.Globalization.NumberStyles.None,
                    System.Globalization.CultureInfo.InvariantCulture, out var level))
                throw Fail(CompanionAppLinkError.PeerDenied);
            var image = new char[32768];
            var size = image.Length;
            if (!AppLinkNative.QueryFullProcessImageName(process, 0, image, ref size))
                throw Fail(CompanionAppLinkError.PeerDenied);
            return new Peer(pid, session, sid.Value, level, container != 0, new string(image, 0, size));
        }
    }

    private static SecurityIdentifier TokenSid(SafeAccessTokenHandle token, int informationClass)
    {
        // TOKEN_USER and TOKEN_MANDATORY_LABEL both start with SID_AND_ATTRIBUTES.
        var buffer = TokenInformation(token, informationClass);
        try
        {
            return new SecurityIdentifier(Marshal.ReadIntPtr(buffer));
        }
        catch (ArgumentException)
        {
            throw Fail(CompanionAppLinkError.PeerDenied);
        }
        finally
        {
            Marshal.FreeHGlobal(buffer);
        }
    }

    private static uint TokenUInt32(SafeAccessTokenHandle token, int informationClass)
    {
        var buffer = Marshal.AllocHGlobal(4);
        try
        {
            if (!AppLinkNative.GetTokenInformation(token, informationClass, buffer, 4, out _))
                throw Fail(CompanionAppLinkError.PeerDenied);
            return (uint)Marshal.ReadInt32(buffer);
        }
        finally
        {
            Marshal.FreeHGlobal(buffer);
        }
    }

    private static IntPtr TokenInformation(SafeAccessTokenHandle token, int informationClass)
    {
        _ = AppLinkNative.GetTokenInformation(token, informationClass, IntPtr.Zero, 0, out var length);
        if (length is 0 or > 65536) throw Fail(CompanionAppLinkError.PeerDenied);
        var buffer = Marshal.AllocHGlobal(length);
        if (AppLinkNative.GetTokenInformation(token, informationClass, buffer, length, out _)) return buffer;
        Marshal.FreeHGlobal(buffer);
        throw Fail(CompanionAppLinkError.PeerDenied);
    }

    // ── Installation registration and the shared switch (HKCU, 64-bit view) ─

    /// <summary>The registered Companion executable, null when not installed.</summary>
    private static string? Registration()
    {
        try
        {
            using var root = RegistryKey.OpenBaseKey(RegistryHive.CurrentUser, RegistryView.Registry64);
            using var key = root.OpenSubKey(RegistrationKey, writable: false);
            if (key is null) return null;
            var value = key.GetValue("ExecutablePath", null, RegistryValueOptions.DoNotExpandEnvironmentNames);
            if (value is null) return null; // retained Enabled after uninstall is still absent
            if (key.GetValueKind("ExecutablePath") != RegistryValueKind.String || value is not string path
                || !LocalExecutablePath(path) || !WithoutReparsePoints(path))
                throw Fail(CompanionAppLinkError.InvalidInstallation);
            return path;
        }
        catch (Exception error) when (error is IOException or UnauthorizedAccessException
            or System.Security.SecurityException or ObjectDisposedException)
        {
            throw Fail(CompanionAppLinkError.InvalidInstallation);
        }
    }

    private static bool EnabledPreference()
    {
        try
        {
            using var root = RegistryKey.OpenBaseKey(RegistryHive.CurrentUser, RegistryView.Registry64);
            using var key = root.OpenSubKey(RegistrationKey, writable: false);
            var value = key?.GetValue("Enabled");
            if (key is null || value is null) return false; // missing means off
            if (key.GetValueKind("Enabled") != RegistryValueKind.DWord || value is not int number || number is not (0 or 1))
                throw Fail(CompanionAppLinkError.InvalidPreference);
            return number == 1;
        }
        catch (Exception error) when (error is IOException or UnauthorizedAccessException
            or System.Security.SecurityException or ObjectDisposedException)
        {
            throw Fail(CompanionAppLinkError.InvalidPreference);
        }
    }

    private static void SetPreference(bool enabled)
    {
        try
        {
            using var root = RegistryKey.OpenBaseKey(RegistryHive.CurrentUser, RegistryView.Registry64);
            using var key = root.OpenSubKey(RegistrationKey, writable: true)
                ?? throw Fail(CompanionAppLinkError.InvalidPreference);
            key.SetValue("Enabled", enabled ? 1 : 0, RegistryValueKind.DWord);
        }
        catch (Exception error) when (error is IOException or UnauthorizedAccessException
            or System.Security.SecurityException or ObjectDisposedException)
        {
            throw Fail(CompanionAppLinkError.InvalidPreference);
        }
    }

    /// <summary>
    /// Only drive-absolute DOS paths (<c>C:\...</c>) to an .exe on a fixed, removable
    /// or RAM drive. UNC, extended/device, forward-slash, alternate-stream, dot and
    /// empty components are refused before any file access.
    /// </summary>
    internal static bool LocalExecutablePath(string path) => LocalFilePath(path, ".exe");

    /// <summary>The same rule for any extension (the carried Companion installer is an .msi).</summary>
    internal static bool LocalFilePath(string path, string extension)
    {
        if (path.Length < 4 || path.Length > 32767 || !char.IsAsciiLetter(path[0]) || path[1] != ':' || path[2] != '\\'
            || path.AsSpan(3).IndexOfAny('/', ':') >= 0 || path.Contains('\0')
            || !path.EndsWith(extension, StringComparison.OrdinalIgnoreCase))
            return false;
        foreach (var part in path[3..].Split('\\'))
            if (part.Length == 0 || part is "." or ".." || part.EndsWith('.') || part.EndsWith(' ')) return false;
        var drive = AppLinkNative.GetDriveType(path[..3]);
        return drive is AppLinkNative.DriveFixed or AppLinkNative.DriveRemovable or AppLinkNative.DriveRamdisk;
    }

    /// <summary>Every component exists and none is a reparse point; the last one is a regular file.</summary>
    internal static bool WithoutReparsePoints(string path)
    {
        for (var i = 3; i <= path.Length; i++)
        {
            if (i != path.Length && path[i] != '\\') continue;
            var attributes = AppLinkNative.GetFileAttributes(path[..i]);
            if (attributes == AppLinkNative.InvalidFileAttributes
                || (attributes & AppLinkNative.FileAttributeReparsePoint) != 0) return false;
            if (i == path.Length && (attributes & (AppLinkNative.FileAttributeDirectory | AppLinkNative.FileAttributeDevice)) != 0)
                return false;
        }
        return true;
    }

    // ── Start on enable: the registered executable, no shell, fixed argument ─

    private static void StartInstalled(string path)
    {
        RequireEligibleCaller(); // never start a user-writable registration elevated
        if (!LocalExecutablePath(path)) throw Fail(CompanionAppLinkError.InvalidInstallation);
        var environment = Marshal.StringToHGlobalUni(LaunchEnvironment());
        try
        {
            var startup = new AppLinkNative.StartupInfo { cb = Marshal.SizeOf<AppLinkNative.StartupInfo>() };
            var commandLine = new StringBuilder($"\"{path}\" {BackgroundArgument}");
            if (!AppLinkNative.CreateProcess(path, commandLine, IntPtr.Zero, IntPtr.Zero, false,
                    AppLinkNative.CreateUnicodeEnvironment, environment, Path.GetDirectoryName(path),
                    ref startup, out var created))
                throw Fail(CompanionAppLinkError.StartFailed);
            AppLinkNative.CloseHandle(created.hThread);
            AppLinkNative.CloseHandle(created.hProcess);
        }
        finally
        {
            Marshal.FreeHGlobal(environment);
        }
    }

    /// <summary>
    /// SystemRoot/WINDIR from the Windows API and the profile folders from this
    /// token's profile; no PATH, provider credential or inherited variable.
    /// </summary>
    private static string LaunchEnvironment()
    {
        var block = new StringBuilder();
        foreach (var (name, value) in LaunchVariables()) block.Append(name).Append('=').Append(value).Append('\0');
        return block.Append('\0').ToString();
    }

    /// <summary>
    /// The variables of <see cref="LaunchEnvironment"/>; the carried-Companion install
    /// (<see cref="CompanionInstaller"/>) starts msiexec with the same set plus a system PATH.
    /// </summary>
    internal static SortedDictionary<string, string> LaunchVariables()
    {
        var variables = new SortedDictionary<string, string>(StringComparer.OrdinalIgnoreCase);
        var windows = new char[260];
        var length = AppLinkNative.GetSystemWindowsDirectory(windows, (uint)windows.Length);
        if (length is > 0 and < 260)
        {
            var root = new string(windows, 0, (int)length);
            variables["SystemRoot"] = root;
            variables["WINDIR"] = root;
        }
        using var self = AppLinkNative.GetCurrentProcess();
        if (AppLinkNative.OpenProcessToken(self, AppLinkNative.TokenQuery, out var token))
        {
            using (token)
            {
                var profile = new char[32767];
                var size = (uint)profile.Length;
                if (AppLinkNative.GetUserProfileDirectory(token, profile, ref size) && size > 1)
                {
                    var home = new string(profile, 0, (int)size - 1);
                    var local = Path.Combine(home, "AppData", "Local");
                    variables["USERPROFILE"] = home;
                    variables["APPDATA"] = Path.Combine(home, "AppData", "Roaming");
                    variables["LOCALAPPDATA"] = local;
                    variables["TEMP"] = Path.Combine(local, "Temp");
                    variables["TMP"] = Path.Combine(local, "Temp");
                }
            }
        }
        else
        {
            token.Dispose();
        }
        return variables;
    }

    // ── Wire: strict v1 frames ─────────────────────────────────────────────

    [GeneratedRegex(@"\A[A-Za-z0-9._:@/+\-]{1,160}\z", RegexOptions.CultureInvariant)]
    private static partial Regex CodePattern();

    [GeneratedRegex(@"\A/[A-Za-z0-9._:@/+\-]{0,255}\z", RegexOptions.CultureInvariant)]
    private static partial Regex RoutePattern();

    internal static bool ValidCode(string? value) => value is not null && CodePattern().IsMatch(value);

    /// <summary>Relative path without query, fragment, host, dot segments or leading //.</summary>
    internal static bool ValidRoute(string? route)
    {
        if (route is null || !RoutePattern().IsMatch(route) || route.StartsWith("//", StringComparison.Ordinal)) return false;
        foreach (var part in route.Split('/')) if (part is "." or "..") return false;
        return true;
    }

    private static bool Absent(string? value) => string.IsNullOrEmpty(value);
    private static bool OptionalCode(string? value) => Absent(value) || ValidCode(value);

    internal static bool ValidContext(CompanionContext? context)
    {
        if (context?.Scope is not { } s || !ValidCode(context.Surface)
            || (!Absent(context.Route) && !ValidRoute(context.Route)) || !OptionalCode(context.Locale))
            return false;
        foreach (var value in new[] { s.ProjectId, s.ServerId, s.EnvironmentId, s.ResourceType, s.ResourceId, s.OfficeId })
            if (!OptionalCode(value)) return false;
        var ok = s.Kind switch
        {
            "none" => AllAbsent(s.ProjectId, s.ServerId, s.EnvironmentId, s.Module, s.ResourceType, s.ResourceId, s.OfficeId),
            "project" => AllAbsent(s.ServerId, s.EnvironmentId, s.Module, s.ResourceType, s.ResourceId, s.OfficeId),
            "server" => AllAbsent(s.ProjectId, s.Module, s.ResourceType, s.ResourceId, s.OfficeId),
            "paperwork" => s.Module is "mail" or "documents" or "meetings" or "notes"
                && AllAbsent(s.ProjectId, s.ServerId, s.EnvironmentId, s.OfficeId),
            "office" => !Absent(s.OfficeId)
                && AllAbsent(s.ProjectId, s.ServerId, s.EnvironmentId, s.Module, s.ResourceType, s.ResourceId),
            _ => false,
        };
        if (!ok) return false;
        if (context.Page is { } page)
        {
            if (!OptionalCode(page.Tool) || !OptionalCode(page.State)) return false;
            if (page.Facts is { } facts)
            {
                if (facts.Count > 16) return false;
                foreach (var (key, value) in facts) if (!ValidCode(key) || !ValidCode(value)) return false;
            }
        }
        return true;
    }

    private static bool AllAbsent(params string?[] values) => values.All(Absent);

    private static byte[] StatusRequest() => Frame(w =>
    {
        Envelope(w, "status", "status.get");
    });

    private static byte[] SwitchRequest(bool enabled) => Frame(w =>
    {
        Envelope(w, "switch", "switch.set");
        w.WriteBoolean("enabled", enabled);
    });

    private static byte[] ContextRequest(string product, CompanionContext context) => Frame(w =>
    {
        Envelope(w, "context", "context.report");
        w.WriteString("product", product);
        w.WriteStartObject("context");
        w.WriteNumber("version", 2);
        w.WriteString("surface", context.Surface);
        Optional(w, "route", context.Route);
        Optional(w, "locale", context.Locale);
        w.WriteStartObject("scope");
        var s = context.Scope;
        w.WriteString("kind", s.Kind);
        Optional(w, "projectId", s.ProjectId);
        Optional(w, "serverId", s.ServerId);
        Optional(w, "environmentId", s.EnvironmentId);
        Optional(w, "module", s.Module);
        Optional(w, "resourceType", s.ResourceType);
        Optional(w, "resourceId", s.ResourceId);
        Optional(w, "officeId", s.OfficeId);
        w.WriteEndObject();
        if (context.Page is { } page)
        {
            w.WriteStartObject("page");
            Optional(w, "tool", page.Tool);
            Optional(w, "state", page.State);
            if (page.Facts is { Count: > 0 } facts)
            {
                w.WriteStartObject("facts");
                foreach (var (key, value) in facts.OrderBy(f => f.Key, StringComparer.Ordinal)) w.WriteString(key, value);
                w.WriteEndObject();
            }
            w.WriteEndObject();
        }
        w.WriteEndObject();
    });

    private static void Envelope(Utf8JsonWriter w, string id, string op)
    {
        w.WriteNumber("version", Version);
        w.WriteString("id", id);
        w.WriteString("op", op);
    }

    private static void Optional(Utf8JsonWriter w, string name, string? value)
    {
        if (!Absent(value)) w.WriteString(name, value);
    }

    private static byte[] Frame(Action<Utf8JsonWriter> body)
    {
        var output = new System.Buffers.ArrayBufferWriter<byte>();
        using (var w = new Utf8JsonWriter(output))
        {
            w.WriteStartObject();
            body(w);
            w.WriteEndObject();
        }
        return output.WrittenSpan.ToArray();
    }

    private sealed record WireResponse(string Id, CompanionStatus? Status, bool? Accepted, string? Error);

    private static readonly UTF8Encoding StrictUtf8 = new(encoderShouldEmitUTF8Identifier: false, throwOnInvalidBytes: true);

    /// <summary>
    /// Exactly the v1 response shapes: valid UTF-8, depth at most 12, no duplicate
    /// keys, no null, no empty string except an error response id, no unknown or
    /// case-variant property, no trailing value, exactly one result.
    /// </summary>
    private static WireResponse ParseResponse(byte[] frame)
    {
        try
        {
            _ = StrictUtf8.GetCharCount(frame);
            Scan(frame);
            using var document = JsonDocument.Parse(frame, new JsonDocumentOptions { MaxDepth = MaxDepth + 1 });
            var root = document.RootElement;
            if (root.ValueKind != JsonValueKind.Object) throw Fail(CompanionAppLinkError.InvalidResponse);
            string? id = null, error = null;
            CompanionStatus? status = null;
            bool? accepted = null;
            var version = false;
            foreach (var property in root.EnumerateObject())
            {
                switch (property.Name)
                {
                    case "version":
                        version = property.Value.ValueKind == JsonValueKind.Number && property.Value.GetRawText() == "1";
                        break;
                    case "id" when property.Value.ValueKind == JsonValueKind.String:
                        id = property.Value.GetString();
                        break;
                    case "status" when property.Value.ValueKind == JsonValueKind.Object:
                        status = ParseStatus(property.Value);
                        break;
                    case "accepted" when property.Value.ValueKind == JsonValueKind.True:
                        accepted = true;
                        break;
                    case "error" when property.Value.ValueKind == JsonValueKind.String
                        && CompanionAppLinkError.Wire.Contains(property.Value.GetString()!):
                        error = property.Value.GetString();
                        break;
                    default:
                        throw Fail(CompanionAppLinkError.InvalidResponse);
                }
            }
            var results = (status is null ? 0 : 1) + (accepted is null ? 0 : 1) + (error is null ? 0 : 1);
            if (!version || id is null || results != 1) throw Fail(CompanionAppLinkError.InvalidResponse);
            if (error is null ? !ValidCode(id) : id.Length > 160) throw Fail(CompanionAppLinkError.InvalidResponse);
            return new WireResponse(id, status, accepted, error);
        }
        catch (Exception error) when (error is JsonException or DecoderFallbackException or ArgumentException
            or InvalidOperationException or FormatException)
        {
            throw Fail(CompanionAppLinkError.InvalidResponse);
        }
    }

    private static CompanionStatus ParseStatus(JsonElement element)
    {
        bool? installed = null, running = null, enabled = null;
        foreach (var property in element.EnumerateObject())
        {
            var value = property.Value.ValueKind switch
            {
                JsonValueKind.True => true,
                JsonValueKind.False => false,
                _ => throw Fail(CompanionAppLinkError.InvalidResponse),
            };
            switch (property.Name)
            {
                case "installed": installed = value; break;
                case "running": running = value; break;
                case "enabled": enabled = value; break;
                default: throw Fail(CompanionAppLinkError.InvalidResponse);
            }
        }
        if (installed is null || running is null || enabled is null) throw Fail(CompanionAppLinkError.InvalidResponse);
        return new CompanionStatus(installed.Value, running.Value, enabled.Value);
    }

    /// <summary>
    /// Structural pass before typed reading: duplicate keys, null, depth and
    /// trailing values. Empty strings are allowed here only as the value of a
    /// root <c>id</c>; the typed pass admits that only on an error response.
    /// </summary>
    private static void Scan(ReadOnlySpan<byte> frame)
    {
        var reader = new Utf8JsonReader(frame, new JsonReaderOptions { MaxDepth = MaxDepth + 1 });
        var keys = new Stack<HashSet<string>>();
        string? property = null;
        while (reader.Read())
        {
            if (reader.CurrentDepth > MaxDepth) throw Fail(CompanionAppLinkError.InvalidResponse);
            switch (reader.TokenType)
            {
                case JsonTokenType.StartObject:
                    keys.Push(new HashSet<string>(StringComparer.Ordinal));
                    break;
                case JsonTokenType.EndObject:
                    keys.Pop();
                    break;
                case JsonTokenType.PropertyName:
                    property = reader.GetString()!;
                    if (!keys.Peek().Add(property)) throw Fail(CompanionAppLinkError.InvalidResponse);
                    continue;
                case JsonTokenType.Null:
                    throw Fail(CompanionAppLinkError.InvalidResponse);
                case JsonTokenType.String when reader.GetString()!.Length == 0:
                    if (!(property == "id" && reader.CurrentDepth == 1)) throw Fail(CompanionAppLinkError.InvalidResponse);
                    break;
            }
            property = null;
        }
        if (keys.Count != 0) throw Fail(CompanionAppLinkError.InvalidResponse);
    }
}

/// <summary>
/// Sends the product's foreground metadata every two seconds while a window of
/// this process is foreground, and nothing otherwise. The product calls
/// <see cref="Stop"/> on minimize, close and sign-out; the host expires a report
/// five seconds after receipt. <c>current</c> must be safe to call from a
/// thread-pool thread (read a snapshot) and returns null when nothing may be sent.
/// </summary>
public sealed class CompanionForegroundReporter(string product, Func<CompanionContext?> current) : IDisposable
{
    private static readonly TimeSpan Interval = TimeSpan.FromSeconds(2);
    private static readonly TimeSpan Ceiling = TimeSpan.FromSeconds(30);
    private readonly object _gate = new();
    private CancellationTokenSource? _loop;

    public void Start()
    {
        if (!CompanionProduct.Valid(product)) throw CompanionAppLink.Fail(CompanionAppLinkError.InvalidRequest);
        lock (_gate)
        {
            if (_loop is not null) return;
            _loop = new CancellationTokenSource();
            _ = RunAsync(_loop.Token);
        }
    }

    public void Stop()
    {
        lock (_gate)
        {
            // Cancelled, not disposed: the running loop may still observe its token.
            _loop?.Cancel();
            _loop = null;
        }
    }

    public void Dispose() => Stop();

    private async Task RunAsync(CancellationToken cancellationToken)
    {
        try
        {
            var delay = Interval;
            while (!cancellationToken.IsCancellationRequested)
            {
                try
                {
                    if (CompanionAppLink.OwnsForegroundWindow() && current() is { } context)
                        await CompanionAppLink.ReportContextAsync(product, context, cancellationToken).ConfigureAwait(false);
                    delay = Interval; // sent, or nothing to send while not foreground
                }
                catch (OperationCanceledException) when (cancellationToken.IsCancellationRequested)
                {
                    throw;
                }
                catch (CompanionAppLinkException error) when (error.Code == CompanionAppLinkError.RateLimited)
                {
                    delay = Interval; // the host's own two-second limit: try at the next tick
                }
                catch (Exception error) when (error is not OutOfMemoryException)
                {
                    // Off, stopped, refused, or the product's context callback failed: the loop
                    // keeps running and backs off to at most 30 seconds while it fails.
                    delay = TimeSpan.FromTicks(Math.Min(delay.Ticks * 2, Ceiling.Ticks));
                }
                await Task.Delay(delay, cancellationToken).ConfigureAwait(false);
            }
        }
        catch (OperationCanceledException)
        {
        }
    }
}

/// <summary>
/// Open-in-app (Windows URL activation, never a pipe message): registers the
/// app's own variant-bound scheme for the Companion and parses an activation URI
/// strictly. The receiving shell foregrounds its single instance and navigates
/// in its existing profile; no login, account switch or action follows.
/// </summary>
public static partial class CompanionOpenInApp
{
    public const string AppsKey = @"Software\kombify\AppLink\Apps";

    [GeneratedRegex(@"\A[a-z0-9-]{1,80}\z", RegexOptions.CultureInvariant)]
    private static partial Regex SchemePattern();

    /// <summary><c>kombify-&lt;product&gt;</c> or <c>kombify-&lt;product&gt;-&lt;suffix&gt;</c>, lowercase, at most 80 characters.</summary>
    public static bool ValidScheme(string product, string? scheme)
    {
        if (!CompanionProduct.Valid(product) || scheme is null || !SchemePattern().IsMatch(scheme)) return false;
        var prefix = "kombify-" + product;
        return scheme == prefix || (scheme.StartsWith(prefix + "-", StringComparison.Ordinal) && scheme.Length > prefix.Length + 1);
    }

    /// <summary>
    /// Writes the per-user Windows URL handler (<c>URL Protocol</c> marker and
    /// <c>shell\open\command</c> to this executable) and the AppLink
    /// <c>Apps\&lt;product&gt;</c> <c>Scheme</c>. Refused for an elevated caller.
    /// </summary>
    public static void Register(string product, string scheme, string executablePath)
    {
        if (!CompanionAppLink.IsCallerEligible())
            throw CompanionAppLink.Fail(CompanionAppLinkError.ElevatedPeerRefused);
        if (!ValidScheme(product, scheme) || !CompanionAppLink.LocalExecutablePath(executablePath))
            throw CompanionAppLink.Fail(CompanionAppLinkError.InvalidRequest);
        try
        {
            using var root = RegistryKey.OpenBaseKey(RegistryHive.CurrentUser, RegistryView.Registry64);
            using (var protocol = root.CreateSubKey($@"Software\Classes\{scheme}", writable: true))
            {
                protocol.SetValue("", "URL:" + scheme, RegistryValueKind.String);
                protocol.SetValue("URL Protocol", "", RegistryValueKind.String);
            }
            using (var command = root.CreateSubKey($@"Software\Classes\{scheme}\shell\open\command", writable: true))
                command.SetValue("", $"\"{executablePath}\" \"%1\"", RegistryValueKind.String);
            using (var app = root.CreateSubKey($@"{AppsKey}\{product}", writable: true))
                app.SetValue("Scheme", scheme, RegistryValueKind.String);
        }
        catch (Exception error) when (error is IOException or UnauthorizedAccessException
            or System.Security.SecurityException or ObjectDisposedException)
        {
            throw CompanionAppLink.Fail(CompanionAppLinkError.AppNotRegistered);
        }
    }

    /// <summary>
    /// Accepts exactly <c>&lt;scheme&gt;://open/?route=&lt;percent-encoded relative route&gt;</c>:
    /// host <c>open</c>, path <c>/</c>, one parameter, decoded once, then the
    /// contract's route rules. Anything else is refused.
    /// </summary>
    public static bool TryParse(string? uri, string scheme, [NotNullWhen(true)] out string? route)
    {
        route = null;
        var prefix = scheme + "://open/?route=";
        if (uri is null || uri.Length > prefix.Length + 3 * 256
            || !uri.StartsWith(prefix, StringComparison.OrdinalIgnoreCase))
            return false;
        var encoded = uri.AsSpan(prefix.Length);
        var decoded = new StringBuilder(encoded.Length);
        for (var i = 0; i < encoded.Length; i++)
        {
            var c = encoded[i];
            if (c == '%')
            {
                if (i + 2 >= encoded.Length || !byte.TryParse(encoded.Slice(i + 1, 2), System.Globalization.NumberStyles.AllowHexSpecifier,
                        System.Globalization.CultureInfo.InvariantCulture, out var value) || value >= 0x80)
                    return false;
                decoded.Append((char)value);
                i += 2;
            }
            else if (char.IsAsciiLetterOrDigit(c) || c is '.' or '-' or '_' or '~')
            {
                decoded.Append(c);
            }
            else
            {
                return false; // '&', '#', '+', '=' and every other raw character: extra parameter or not canonical
            }
        }
        var candidate = decoded.ToString();
        if (!CompanionAppLink.ValidRoute(candidate)) return false;
        route = candidate;
        return true;
    }
}

internal static class AppLinkNative
{
    public const uint GenericRead = 0x80000000;
    public const uint GenericWrite = 0x40000000;
    public const uint OpenExisting = 3;
    public const uint FileFlagOverlapped = 0x40000000;
    public const uint SecuritySqosPresent = 0x00100000;
    public const uint SecurityIdentification = 0x00010000;
    public const int ErrorFileNotFound = 2;
    public const int ErrorPathNotFound = 3;
    public const int ErrorAccessDenied = 5;
    public const int ErrorPipeBusy = 231;
    public const uint ProcessQueryLimitedInformation = 0x1000;
    public const uint TokenQuery = 0x0008;
    public const int TokenUser = 1;
    public const int TokenSessionId = 12;
    public const int TokenIntegrityLevel = 25;
    public const int TokenIsAppContainer = 29;
    public const uint InvalidFileAttributes = 0xFFFFFFFF;
    public const uint FileAttributeDirectory = 0x10;
    public const uint FileAttributeDevice = 0x40;
    public const uint FileAttributeReparsePoint = 0x400;
    public const uint DriveRemovable = 2;
    public const uint DriveFixed = 3;
    public const uint DriveRamdisk = 6;
    public const uint CreateUnicodeEnvironment = 0x00000400;

    [StructLayout(LayoutKind.Sequential, CharSet = CharSet.Unicode)]
    public struct StartupInfo
    {
        public int cb;
        public IntPtr lpReserved;
        public IntPtr lpDesktop;
        public IntPtr lpTitle;
        public int dwX;
        public int dwY;
        public int dwXSize;
        public int dwYSize;
        public int dwXCountChars;
        public int dwYCountChars;
        public int dwFillAttribute;
        public int dwFlags;
        public short wShowWindow;
        public short cbReserved2;
        public IntPtr lpReserved2;
        public IntPtr hStdInput;
        public IntPtr hStdOutput;
        public IntPtr hStdError;
    }

    [StructLayout(LayoutKind.Sequential)]
    public struct ProcessInformation
    {
        public IntPtr hProcess;
        public IntPtr hThread;
        public int dwProcessId;
        public int dwThreadId;
    }

    [DllImport("kernel32.dll", EntryPoint = "CreateFileW", CharSet = CharSet.Unicode, SetLastError = true)]
    public static extern SafePipeHandle CreateFile(string name, uint access, uint share, IntPtr security,
        uint creation, uint flags, IntPtr template);

    [DllImport("kernel32.dll", SetLastError = true)]
    [return: MarshalAs(UnmanagedType.Bool)]
    public static extern bool GetNamedPipeServerProcessId(SafePipeHandle pipe, out uint serverProcessId);

    [DllImport("kernel32.dll", SetLastError = true)]
    public static extern SafeProcessHandle OpenProcess(uint access, [MarshalAs(UnmanagedType.Bool)] bool inherit, uint pid);

    [DllImport("kernel32.dll")]
    public static extern SafeProcessHandle GetCurrentProcess();

    [DllImport("kernel32.dll", EntryPoint = "QueryFullProcessImageNameW", CharSet = CharSet.Unicode, SetLastError = true)]
    [return: MarshalAs(UnmanagedType.Bool)]
    public static extern bool QueryFullProcessImageName(SafeProcessHandle process, uint flags,
        [Out] char[] name, ref int size);

    [DllImport("advapi32.dll", SetLastError = true)]
    [return: MarshalAs(UnmanagedType.Bool)]
    public static extern bool OpenProcessToken(SafeProcessHandle process, uint access, out SafeAccessTokenHandle token);

    [DllImport("advapi32.dll", SetLastError = true)]
    [return: MarshalAs(UnmanagedType.Bool)]
    public static extern bool GetTokenInformation(SafeAccessTokenHandle token, int informationClass,
        IntPtr information, int length, out int returnLength);

    [DllImport("userenv.dll", EntryPoint = "GetUserProfileDirectoryW", CharSet = CharSet.Unicode, SetLastError = true)]
    [return: MarshalAs(UnmanagedType.Bool)]
    public static extern bool GetUserProfileDirectory(SafeAccessTokenHandle token, [Out] char[] path, ref uint size);

    [DllImport("kernel32.dll", EntryPoint = "GetSystemWindowsDirectoryW", CharSet = CharSet.Unicode, SetLastError = true)]
    public static extern uint GetSystemWindowsDirectory([Out] char[] buffer, uint size);

    [DllImport("kernel32.dll", EntryPoint = "GetFileAttributesW", CharSet = CharSet.Unicode, SetLastError = true)]
    public static extern uint GetFileAttributes(string path);

    [DllImport("kernel32.dll", EntryPoint = "GetDriveTypeW", CharSet = CharSet.Unicode)]
    public static extern uint GetDriveType(string root);

    [DllImport("kernel32.dll", EntryPoint = "CreateProcessW", CharSet = CharSet.Unicode, SetLastError = true)]
    [return: MarshalAs(UnmanagedType.Bool)]
    public static extern bool CreateProcess(string applicationName, StringBuilder commandLine, IntPtr processAttributes,
        IntPtr threadAttributes, [MarshalAs(UnmanagedType.Bool)] bool inheritHandles, uint creationFlags,
        IntPtr environment, string? currentDirectory, ref StartupInfo startupInfo, out ProcessInformation processInformation);

    [DllImport("kernel32.dll", SetLastError = true)]
    [return: MarshalAs(UnmanagedType.Bool)]
    public static extern bool CloseHandle(IntPtr handle);

    [DllImport("user32.dll")]
    public static extern IntPtr GetForegroundWindow();

    [DllImport("user32.dll")]
    public static extern uint GetWindowThreadProcessId(IntPtr window, out uint processId);
}
