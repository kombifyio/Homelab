using System.ComponentModel;
using System.Diagnostics;
using System.Runtime.InteropServices;
using System.Security.Cryptography;
using System.Text;

namespace Kombify.Client.Shell;

// Installs the desktop Companion from the one installer a kombify Windows client
// carries beside itself (platform-2oj0se). The client pins that installer by
// version and SHA-256 and builds it into its own installer; nothing is ever
// downloaded. The install is per user and never elevated. Whether a Companion is
// already there is Windows Installer's answer under the fixed UpgradeCode
// (AppLink v2 "Installation, registration, removal"): the same or a newer version is left as it is, an
// older one is upgraded in place by the MSI's own MajorUpgrade, none is
// installed. The MSI is the hidden system component of the contract (no Start
// menu entry, no Apps list entry). The pinned digest is verified on a handle
// that refuses writers until msiexec has exited.

/// <summary>
/// The carried Companion installer: an absolute local .msi path, its version (<c>major.minor.build</c>,
/// the MSI ProductVersion) and pinned SHA-256 (lowercase hex).
/// </summary>
public sealed record CompanionPayload(string Path, string Version, string Sha256);

/// <summary>
/// The end of one install attempt. <see cref="Code"/> is one of <see cref="CompanionInstallCode"/>;
/// <see cref="ExitCode"/> is msiexec's when it ran. No path, OS message or log content crosses it.
/// </summary>
public sealed record CompanionInstallResult(string Code, int? ExitCode = null)
{
    /// <summary>A Companion of the payload's version or newer is registered now: installed or upgraded by this attempt, or already there.</summary>
    public bool Registered => Code is CompanionInstallCode.Installed or CompanionInstallCode.AlreadyRegistered;

    /// <summary>
    /// Not a lasting failure: another install was running (msiexec 1618) or the
    /// install was cancelled (1602). The caller re-reads and may offer it again.
    /// </summary>
    public bool Transient => Code is CompanionInstallCode.InstallBusy or CompanionInstallCode.InstallCancelled;
}

public static class CompanionInstallCode
{
    public const string Installed = "installed";
    public const string AlreadyRegistered = "already_registered";
    public const string CallerRefused = "caller_refused";
    public const string PayloadMissing = "payload_missing";
    public const string PayloadMismatch = "payload_mismatch";
    public const string LogUnavailable = "log_unavailable";
    public const string StartFailed = "start_failed";
    public const string InstallBusy = "install_busy";
    public const string InstallCancelled = "install_cancelled";
    public const string InstallFailed = "install_failed";
    public const string InstallTimeout = "install_timeout";
    /// <summary>Windows Installer could not say which Companion is installed; nothing was run.</summary>
    public const string InstallerUnknown = "installer_unknown";
}

public static class CompanionInstaller
{
    /// <summary>The Companion MSI's UpgradeCode (kombify-AI-Platform speechkit/installer/wix/Companion.wxs), fixed for good.</summary>
    public const string UpgradeCode = "{16167BF4-5DFC-4CF5-8A93-5F89D0F72B39}";

    private const int ErrorInstallUserExit = 1602;
    private const int ErrorInstallAlreadyRunning = 1618;
    private static readonly TimeSpan InstallDeadline = TimeSpan.FromMinutes(10);
    private static readonly object Gate = new();
    private static Task<CompanionInstallResult>? s_running;
    private static (string Path, long Length, DateTime Written, bool Matches)? s_checked;

    /// <summary>True while an install this process started is still running.</summary>
    public static bool Installing
    {
        get { lock (Gate) return s_running is { IsCompleted: false }; }
    }

    /// <summary>
    /// The highest Companion version Windows Installer has registered under the UpgradeCode
    /// for this user or the machine; null when none is. Throws <see cref="InvalidOperationException"/>
    /// when Windows Installer cannot answer.
    /// </summary>
    public static Version? InstalledVersion()
    {
        Version? highest = null;
        for (uint index = 0; ; index++)
        {
            var product = new StringBuilder(39);
            var result = MsiNative.MsiEnumRelatedProducts(UpgradeCode, 0, index, product);
            if (result == MsiNative.ErrorNoMoreItems) return highest;
            if (result != MsiNative.ErrorSuccess || index > 64) throw new InvalidOperationException("msi_enum");
            var buffer = new StringBuilder(64);
            var length = (uint)buffer.Capacity;
            if (MsiNative.MsiGetProductInfo(product.ToString(), "VersionString", buffer, ref length) != MsiNative.ErrorSuccess
                || ParseVersion(buffer.ToString()) is not { } version)
                throw new InvalidOperationException("msi_version");
            if (highest is null || version > highest) highest = version;
        }
    }

    /// <summary>
    /// The install offer of rule 5 without the interface read the caller owns: an
    /// eligible (medium-integrity, not elevated) caller, no Companion of the payload's
    /// version or newer registered with Windows Installer, and the carried payload present
    /// with the pinned digest. Enumerates Windows Installer and may hash the payload, so
    /// call it off the UI thread. Never throws.
    /// </summary>
    public static bool CanInstall(CompanionPayload payload)
    {
        try
        {
            return CompanionAppLink.IsCallerEligible() && Presence(payload) is Present.None or Present.Older
                && PayloadMatches(payload);
        }
        catch (Exception error) when (error is not OutOfMemoryException)
        {
            return false;
        }
    }

    /// <summary>
    /// Installs the carried payload for this Windows user with no UI and a verbose
    /// msiexec log at <paramref name="logPath"/>. One install at a time per process:
    /// a call while one runs gets that same attempt's result. Never throws; the
    /// caller re-reads the Companion through <see cref="CompanionAppLink"/> afterwards,
    /// whatever the result.
    /// </summary>
    public static Task<CompanionInstallResult> InstallAsync(CompanionPayload payload, string logPath)
    {
        lock (Gate)
        {
            if (s_running is { IsCompleted: false } running) return running;
            return s_running = Task.Run(() => Install(payload, logPath));
        }
    }

    private static CompanionInstallResult Install(CompanionPayload payload, string logPath)
    {
        FileStream? package = null;
        Process? msiexec = null;
        var handedOff = false;
        try
        {
            if (!CompanionAppLink.IsCallerEligible()) return new(CompanionInstallCode.CallerRefused);
            if (!ValidPayload(payload)) return new(CompanionInstallCode.PayloadMissing);
            switch (Presence(payload))
            {
                case Present.SameOrNewer: return new(CompanionInstallCode.AlreadyRegistered);
                case Present.Unknown: return new(CompanionInstallCode.InstallerUnknown);
            }
            if (!PrepareLog(logPath)) return new(CompanionInstallCode.LogUnavailable);

            try
            {
                // Read-only handle that refuses writers and deletion: the bytes hashed are the bytes msiexec reads.
                package = new FileStream(payload.Path, FileMode.Open, FileAccess.Read, FileShare.Read);
            }
            catch (Exception error) when (error is IOException or UnauthorizedAccessException)
            {
                return new(CompanionInstallCode.PayloadMissing);
            }
            if (!DigestMatches(package, payload.Sha256)) return new(CompanionInstallCode.PayloadMismatch);

            try
            {
                msiexec = Process.Start(InstallStart(payload.Path, logPath));
            }
            catch (Win32Exception)
            {
                return new(CompanionInstallCode.StartFailed);
            }
            if (msiexec is null) return new(CompanionInstallCode.StartFailed);
            if (!msiexec.WaitForExit(InstallDeadline))
            {
                // A timed-out msiexec is left running: the Windows Installer service finishes or
                // rolls back on its own. The handle stays open until msiexec has exited.
                var (running, held) = (msiexec, package);
                _ = Task.Run(() =>
                {
                    try { running.WaitForExit(); }
                    finally { held.Dispose(); running.Dispose(); }
                });
                handedOff = true;
                return new(CompanionInstallCode.InstallTimeout);
            }
            var exit = msiexec.ExitCode;
            if (exit == ErrorInstallAlreadyRunning) return new(CompanionInstallCode.InstallBusy, exit);
            if (exit == ErrorInstallUserExit) return new(CompanionInstallCode.InstallCancelled, exit);
            // 3010 and 1641: installed, a restart is pending (the Companion needs none).
            var succeeded = exit is 0 or 3010 or 1641;
            return succeeded && Presence(payload) == Present.SameOrNewer
                ? new(CompanionInstallCode.Installed, exit)
                : new(CompanionInstallCode.InstallFailed, exit);
        }
        catch (Exception error) when (error is not OutOfMemoryException)
        {
            return new(CompanionInstallCode.InstallFailed);
        }
        finally
        {
            if (!handedOff)
            {
                package?.Dispose();
                msiexec?.Dispose();
            }
        }
    }

    /// <summary>
    /// The system msiexec, per user (<c>ALLUSERS=""</c>), no UI, no restart, a verbose
    /// log. Its environment is cleared to the Companion start's set
    /// (<see cref="CompanionAppLink.LaunchVariables"/>) plus a PATH of the system
    /// directory; nothing of this process's environment reaches it. Windows paths
    /// cannot contain a quote, so quoting them is enough.
    /// </summary>
    private static ProcessStartInfo InstallStart(string package, string logPath)
    {
        var system = Environment.SystemDirectory;
        var start = new ProcessStartInfo(Path.Combine(system, "msiexec.exe"))
        {
            UseShellExecute = false,
            CreateNoWindow = true,
            WorkingDirectory = system,
            Arguments = $"/i \"{package}\" ALLUSERS=\"\" /qn /norestart /l*v \"{logPath}\"",
        };
        start.Environment.Clear();
        foreach (var (name, value) in CompanionAppLink.LaunchVariables()) start.Environment[name] = value;
        start.Environment["PATH"] = system;
        return start;
    }

    /// <summary>An absolute local .log path whose directory exists or can be created.</summary>
    private static bool PrepareLog(string logPath)
    {
        if (!CompanionAppLink.LocalFilePath(logPath, ".log") || logPath.Contains('"')) return false;
        try
        {
            Directory.CreateDirectory(Path.GetDirectoryName(logPath)!);
            return true;
        }
        catch (Exception error) when (error is IOException or UnauthorizedAccessException or NotSupportedException)
        {
            return false;
        }
    }

    /// <summary>The payload exists as a regular local .msi with the pinned digest; checked again only when its size or time changes.</summary>
    private static bool PayloadMatches(CompanionPayload payload)
    {
        if (!ValidPayload(payload)) return false;
        var file = new FileInfo(payload.Path);
        if (!file.Exists) return false;
        lock (Gate)
        {
            if (s_checked is { } seen && seen.Path == payload.Path && seen.Length == file.Length
                && seen.Written == file.LastWriteTimeUtc) return seen.Matches;
        }
        using var package = new FileStream(payload.Path, FileMode.Open, FileAccess.Read, FileShare.Read);
        var matches = DigestMatches(package, payload.Sha256);
        lock (Gate) s_checked = (payload.Path, file.Length, file.LastWriteTimeUtc, matches);
        return matches;
    }

    // The reparse-point walk runs before the handle is opened; a same-user process could swap a component in
    // between, which gains nothing: the digest is checked on the opened handle, and the install runs as that user.
    private static bool ValidPayload(CompanionPayload payload) =>
        ParseVersion(payload.Version) is not null && payload.Sha256.Length == 64 && payload.Sha256.All(c => c is >= '0' and <= '9' or >= 'a' and <= 'f')
        && CompanionAppLink.LocalFilePath(payload.Path, ".msi") && CompanionAppLink.WithoutReparsePoints(payload.Path);

    private enum Present { None, Older, SameOrNewer, Unknown }

    /// <summary>Windows Installer's answer for the payload's version; an unknown state never installs.</summary>
    private static Present Presence(CompanionPayload payload)
    {
        if (ParseVersion(payload.Version) is not { } carried) return Present.Unknown;
        Version? installed;
        try
        {
            installed = InstalledVersion();
        }
        catch (InvalidOperationException)
        {
            return Present.Unknown;
        }
        return installed is null ? Present.None : installed >= carried ? Present.SameOrNewer : Present.Older;
    }

    /// <summary>
    /// An MSI ProductVersion: <c>major.minor.build</c> (an optional fourth field is ignored, as
    /// Windows Installer ignores it for upgrades), each field decimal digits within MSI's limits.
    /// </summary>
    internal static Version? ParseVersion(string? text)
    {
        if (text is null || text.Length > 32) return null;
        var parts = text.Split('.');
        if (parts.Length is < 3 or > 4) return null;
        var fields = new int[3];
        for (var i = 0; i < parts.Length; i++)
        {
            if (parts[i].Length is 0 or > 5 || !parts[i].All(char.IsAsciiDigit)) return null;
            var value = int.Parse(parts[i], System.Globalization.CultureInfo.InvariantCulture);
            if (i < 3) fields[i] = value;
        }
        if (fields[0] > 255 || fields[1] > 255 || fields[2] > 65535) return null;
        return new Version(fields[0], fields[1], fields[2]);
    }

    private static bool DigestMatches(FileStream package, string sha256)
    {
        package.Position = 0;
        var actual = Convert.ToHexStringLower(SHA256.HashData(package));
        return CryptographicOperations.FixedTimeEquals(Encoding.ASCII.GetBytes(actual), Encoding.ASCII.GetBytes(sha256));
    }
}

internal static class MsiNative
{
    public const uint ErrorSuccess = 0;
    public const uint ErrorNoMoreItems = 259;

    [DllImport("msi.dll", EntryPoint = "MsiEnumRelatedProductsW", CharSet = CharSet.Unicode)]
    public static extern uint MsiEnumRelatedProducts(string upgradeCode, uint reserved, uint index, StringBuilder productBuffer);

    [DllImport("msi.dll", EntryPoint = "MsiGetProductInfoW", CharSet = CharSet.Unicode)]
    public static extern uint MsiGetProductInfo(string product, string property, StringBuilder valueBuffer, ref uint length);
}
