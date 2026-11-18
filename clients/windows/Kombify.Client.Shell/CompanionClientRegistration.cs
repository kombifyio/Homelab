using Microsoft.Win32;

namespace Kombify.Client.Shell;

/// <summary>
/// A client's own registration with the Companion (AppLink v2 "Installation, registration, removal"):
/// <c>HKCU\Software\kombify\CompanionDesktop\Clients</c> (64-bit view), value
/// <c>&lt;product&gt;</c> = REG_SZ with the absolute path of the client's executable.
/// The Companion admits the client that switched it on by matching its OS-proven
/// image against this value, and removes itself once no listed executable exists.
/// A client writes it at install where its installer can and at every switch-on
/// (<see cref="CompanionAppLink.SetEnabledAsync"/> does), and deletes only its own
/// value when its uninstaller can. Refused for an elevated, low-integrity or
/// AppContainer caller, like every AppLink call.
/// </summary>
public static class CompanionClientRegistration
{
    public const string ClientsKey = CompanionAppLink.RegistrationKey + @"\Clients";

    /// <summary>
    /// Registers this process's executable for <paramref name="product"/>. The path is the
    /// one Windows reports for this process (<c>QueryFullProcessImageName</c>), the same
    /// evidence the Companion reads from a connecting peer, so the two always compare equal.
    /// </summary>
    public static void Register(string product)
    {
        CompanionAppLink.RequireEligibleCaller();
        Register(product, CompanionAppLink.LocalPeer().Executable);
    }

    /// <summary>Registers <paramref name="executablePath"/> (an absolute local .exe) for <paramref name="product"/>.</summary>
    public static void Register(string product, string executablePath)
    {
        CompanionAppLink.RequireEligibleCaller();
        if (!CompanionProduct.Valid(product) || !CompanionAppLink.LocalExecutablePath(executablePath))
            throw CompanionAppLink.Fail(CompanionAppLinkError.InvalidRequest);
        try
        {
            using var root = RegistryKey.OpenBaseKey(RegistryHive.CurrentUser, RegistryView.Registry64);
            using var clients = root.CreateSubKey(ClientsKey, writable: true);
            if (clients.GetValue(product, null, RegistryValueOptions.DoNotExpandEnvironmentNames) is string current
                && clients.GetValueKind(product) == RegistryValueKind.String
                && string.Equals(current, executablePath, StringComparison.Ordinal))
                return; // already this value: an unchanged registry on every switch-on
            clients.SetValue(product, executablePath, RegistryValueKind.String);
        }
        catch (Exception error) when (error is IOException or UnauthorizedAccessException
            or System.Security.SecurityException or ObjectDisposedException)
        {
            throw CompanionAppLink.Fail(CompanionAppLinkError.AppNotRegistered);
        }
    }

    /// <summary>
    /// Deletes this client's own value; a missing value or key is already the result. The
    /// <c>Clients</c> key itself stays, so the Companion still reads "no client left" from it.
    /// </summary>
    public static void Unregister(string product)
    {
        CompanionAppLink.RequireEligibleCaller();
        if (!CompanionProduct.Valid(product)) throw CompanionAppLink.Fail(CompanionAppLinkError.InvalidRequest);
        try
        {
            using var root = RegistryKey.OpenBaseKey(RegistryHive.CurrentUser, RegistryView.Registry64);
            using var clients = root.OpenSubKey(ClientsKey, writable: true);
            clients?.DeleteValue(product, throwOnMissingValue: false);
        }
        catch (Exception error) when (error is IOException or UnauthorizedAccessException
            or System.Security.SecurityException or ObjectDisposedException)
        {
            throw CompanionAppLink.Fail(CompanionAppLinkError.AppNotRegistered);
        }
    }
}

/// <summary>
/// The watch loop of AppLink v2 ("Change notification") for a client while it shows the
/// Companion section: reads the snapshot, holds <c>settings.watch</c>, reads again
/// when the revision rose. On a lost transport or a stopped program it calls
/// <c>status.get</c> (<see cref="CompanionAppLink.DiscoverAsync"/>) and backs off
/// 1 s, 2 s, 5 s, 10 s, 20 s, then 30 s. The back-off resets after a watch reports a
/// risen revision or answers after at least one second. A refused watch
/// (<c>rate_limited</c>, the host's watch limit) or a quick answer whose revision did not
/// rise spends a back-off step. A lower revision is refused by the transport and
/// recovers through discovery, back-off and a fresh read. <c>changed</c>
/// gets every snapshot that differs in revision or status from the last one it got
/// (the stopped snapshot included), on a thread-pool thread; marshal to the UI
/// thread yourself. Start when the section shows, Stop when it hides.
/// </summary>
public sealed class CompanionSettingsWatcher(Action<CompanionSettings> changed) : IDisposable
{
    /// <summary>The back-off after a failed read/watch, a stopped program or a quick unchanged answer.</summary>
    public static IReadOnlyList<TimeSpan> BackOff { get; } =
        [TimeSpan.FromSeconds(1), TimeSpan.FromSeconds(2), TimeSpan.FromSeconds(5),
         TimeSpan.FromSeconds(10), TimeSpan.FromSeconds(20), TimeSpan.FromSeconds(30)];

    /// <summary>The shortest watch without a risen revision that resets the back-off.</summary>
    private static readonly TimeSpan HeldWatch = TimeSpan.FromSeconds(1);

    private readonly object _gate = new();
    private CancellationTokenSource? _loop;

    public void Start()
    {
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
        CompanionSettings? last = null;
        ulong? watching = null;
        var failures = 0;
        void Emit(CompanionSettings settings)
        {
            // Never after Stop(): a snapshot read before the cancellation is dropped.
            if (cancellationToken.IsCancellationRequested) return;
            if (last is not null && last.Revision == settings.Revision && last.Status == settings.Status) return;
            last = settings;
            try { changed(settings); }
            catch (Exception error) when (error is not OutOfMemoryException) { } // the client's callback never stops the loop
        }
        try
        {
            while (!cancellationToken.IsCancellationRequested)
            {
                try
                {
                    if (watching is null)
                    {
                        var settings = await CompanionAppLink.GetSettingsAsync(cancellationToken).ConfigureAwait(false);
                        Emit(settings);
                        if (!settings.Running)
                        {
                            // Stopped or not installed: nothing to watch; look again after the back-off.
                            await Task.Delay(BackOff[Math.Min(failures++, BackOff.Count - 1)], cancellationToken).ConfigureAwait(false);
                            continue;
                        }
                        watching = settings.Revision;
                    }
                    var started = System.Diagnostics.Stopwatch.GetTimestamp();
                    var revision = await CompanionAppLink.WatchSettingsAsync(watching.Value, cancellationToken).ConfigureAwait(false);
                    var held = System.Diagnostics.Stopwatch.GetElapsedTime(started);
                    if (revision > watching.Value || held >= HeldWatch) failures = 0;
                    else
                    {
                        await Task.Delay(BackOff[Math.Min(failures++, BackOff.Count - 1)], cancellationToken).ConfigureAwait(false);
                    }
                    if (revision != watching)
                    {
                        watching = null; // rose: read the snapshot, then watch again
                    }
                }
                catch (OperationCanceledException) when (cancellationToken.IsCancellationRequested)
                {
                    throw;
                }
                catch (Exception error) when (error is not OutOfMemoryException)
                {
                    watching = null;
                    try
                    {
                        var status = await CompanionAppLink.DiscoverAsync(cancellationToken).ConfigureAwait(false);
                        if (!status.Running) Emit(CompanionSettings.Stopped(status.Installed, status.Enabled));
                    }
                    catch (OperationCanceledException) when (cancellationToken.IsCancellationRequested)
                    {
                        throw;
                    }
                    catch (Exception statusError) when (statusError is not OutOfMemoryException)
                    {
                        // Refused or unreadable: the last snapshot stays; try again after the back-off.
                    }
                    await Task.Delay(BackOff[Math.Min(failures++, BackOff.Count - 1)], cancellationToken).ConfigureAwait(false);
                }
            }
        }
        catch (OperationCanceledException)
        {
        }
    }
}
