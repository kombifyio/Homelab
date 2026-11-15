using System.ComponentModel;
using System.Diagnostics;
using System.Runtime.InteropServices;
using System.Security.Cryptography;
using System.Text;

namespace Kombify.Client.Shell;

// Installs the desktop Companion from the one installer a kombify Windows client
// carries beside itself (platform-2oj0se). The client pins that installer by
// version and SHA-256 and builds it into its own installer; nothing is ever
// downloaded. The install is per user and never elevated, runs only when no
// Companion is registered with Windows Installer (an installed one, newer or
// older, is never replaced here), and verifies the pinned digest on a handle
// that refuses writers until msiexec has exited.

/// <summary>The carried Companion installer: an absolute local .msi path, its version and pinned SHA-256 (lowercase hex).</summary>
public sealed record CompanionPayload(string Path, string Version, string Sha256);

/// <summary>
/// The end of one install attempt. <see cref="Code"/> is one of <see cref="CompanionInstallCode"/>;
/// <see cref="ExitCode"/> is msiexec's when it ran. No path, OS message or log content crosses it.
/// </summary>
public sealed record CompanionInstallResult(string Code, int? ExitCode = null)
{
    /// <summary>A Companion is registered now: installed by this attempt or already there.</summary>
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
    /// True when Windows Installer knows a Companion product for this user or the
    /// machine, of any version. Also true when Windows Installer cannot answer, so
    /// an unknown state never offers an install.
    /// </summary>
    public static bool IsProductRegistered()
    {
        var product = new StringBuilder(39);
        var result = MsiNative.MsiEnumRelatedProducts(UpgradeCode, 0, 0, product);
        return result != MsiNative.ErrorNoMoreItems;
    }

    /// <summary>
    /// The install offer of rule 5 without the interface read the caller owns: an
    /// eligible (medium-integrity, not elevated) caller, no registered Companion,
    /// and the carried payload present with the pinned digest. Enumerates Windows
    /// Installer and may hash the payload, so call it off the UI thread. Never throws.
    /// </summary>
    public static bool CanInstall(CompanionPayload payload)
    {
        try
        {
            return CompanionAppLink.IsCallerEligible() && !IsProductRegistered() && PayloadMatches(payload);
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
            if (IsProductRegistered()) return new(CompanionInstallCode.AlreadyRegistered);
            if (!ValidPayload(payload)) return new(CompanionInstallCode.PayloadMissing);
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
            return succeeded && IsProductRegistered()
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
        payload.Sha256.Length == 64 && payload.Sha256.All(c => c is >= '0' and <= '9' or >= 'a' and <= 'f')
        && CompanionAppLink.LocalFilePath(payload.Path, ".msi") && CompanionAppLink.WithoutReparsePoints(payload.Path);

    private static bool DigestMatches(FileStream package, string sha256)
    {
        package.Position = 0;
        var actual = Convert.ToHexStringLower(SHA256.HashData(package));
        return CryptographicOperations.FixedTimeEquals(Encoding.ASCII.GetBytes(actual), Encoding.ASCII.GetBytes(sha256));
    }
}

internal static class MsiNative
{
    public const uint ErrorNoMoreItems = 259;

    [DllImport("msi.dll", EntryPoint = "MsiEnumRelatedProductsW", CharSet = CharSet.Unicode)]
    public static extern uint MsiEnumRelatedProducts(string upgradeCode, uint reserved, uint index, StringBuilder productBuffer);
}
