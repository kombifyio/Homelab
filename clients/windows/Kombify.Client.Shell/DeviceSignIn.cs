using System.ComponentModel;
using System.Globalization;
using System.Runtime.InteropServices;
using System.Security.Cryptography;
using System.Security.Principal;
using System.Text;
using Microsoft.Win32;
using Microsoft.Win32.SafeHandles;

namespace Kombify.Client.Shell;

// One sign-in per device (workspace platform/standards/desktop-device-sign-in.v1.md).
// The kombify desktop programs of one Windows user share only whether this user is signed
// in, and as whom: HKCU\Software\kombify\DesktopSignIn in the 64-bit view. No token, code,
// cookie or raw account identifier is ever stored there; every program keeps its own
// registration, tokens and refresh. Elevated (or AppContainer) processes neither read nor
// write it: every entry point below is a no-op for them.

/// <summary>What the shared key says.</summary>
public enum DeviceSignInKind
{
    /// <summary>No key, no value or a value of the wrong type (contract §1).</summary>
    Absent,
    SignedIn,
    SignedOut,
    /// <summary>A <c>Version</c> other than 1: neither acted on nor written.</summary>
    NotReadable,
}

/// <summary>One consistent read of the shared state (never two writes mixed).</summary>
public sealed record DeviceSignInState(DeviceSignInKind Kind, string Subject, string Label, ulong Revision, string ChangedBy)
{
    internal static DeviceSignInState AbsentState { get; } = new(DeviceSignInKind.Absent, "", "", 0, "");
    internal static DeviceSignInState Unreadable { get; } = new(DeviceSignInKind.NotReadable, "", "", 0, "");
}

/// <summary>
/// The answer of a write that waited for the mutex: <see cref="Written"/> false means it was dropped
/// (the state is not readable, or someone wrote since the write was queued) and must not be retried;
/// <see cref="State"/> is what the key holds afterwards.
/// </summary>
public sealed record DeviceSignInWriteResult(bool Written, DeviceSignInState State);

/// <summary>An account as the shared state names it: the subject hash and the display label.</summary>
public sealed record DeviceSignInAccount(string Subject, string Label)
{
    /// <summary>
    /// The account of an ID token: <paramref name="issuer"/> exactly as issued
    /// (<see cref="DeviceSignIn.KombifyIssuer"/>), <paramref name="subject"/> the raw <c>sub</c>,
    /// <paramref name="label"/> the token's <c>email</c>, else its <c>name</c>.
    /// </summary>
    public static DeviceSignInAccount FromIdToken(string issuer, string subject, string? email, string? name) =>
        new(DeviceSignIn.SubjectHash(issuer, subject), DeviceSignIn.NormalizeLabel(
            !string.IsNullOrWhiteSpace(email) ? email : name ?? ""));
}

/// <summary>The products that write the state (<c>ChangedBy</c>).</summary>
public static class DeviceSignInProduct
{
    public const string Workbench = "workbench";
    public const string Paperwork = "paperwork";
    public const string Techstack = "techstack";
    public const string Companion = "companion";

    internal static bool Valid(string? product) => product is Workbench or Paperwork or Techstack or Companion;
}

/// <summary>Reads and writes the shared state (contract §1 and §2).</summary>
public static class DeviceSignIn
{
    public const string RegistryKeyPath = @"Software\kombify\DesktopSignIn";
    /// <summary>The <c>iss</c> of the kombify tenant's ID tokens, exactly as issued.</summary>
    public const string KombifyIssuer = "https://login.kombify.io/";
    public const int MaxLabelLength = 254;

    private const string SignedInValue = "signed_in";
    private const string SignedOutValue = "signed_out";
    private static readonly TimeSpan MutexWait = TimeSpan.FromSeconds(2);
    private static readonly Lazy<bool> Eligible = new(CompanionAppLink.IsCallerEligible);
    private static readonly Lazy<string?> MutexName = new(() =>
    {
        try
        {
            using var identity = WindowsIdentity.GetCurrent();
            return identity.User is { } sid ? @"Global\kombify.desktop-sign-in.v1." + sid.Value : null;
        }
        catch (Exception error) when (error is System.Security.SecurityException or UnauthorizedAccessException)
        {
            return null;
        }
    });

    /// <summary>False for an elevated or AppContainer process: it neither reads nor writes (§7).</summary>
    public static bool IsCallerEligible => Eligible.Value;

    /// <summary>base64url (no padding) of SHA-256 over the UTF-8 bytes of <c>&lt;iss&gt;|&lt;sub&gt;</c>.</summary>
    public static string SubjectHash(string issuer, string subject)
    {
        if (string.IsNullOrEmpty(issuer) || string.IsNullOrEmpty(subject))
            throw new ArgumentException("An account needs its issuer and subject.");
        return Convert.ToBase64String(SHA256.HashData(Encoding.UTF8.GetBytes(issuer + "|" + subject)))
            .TrimEnd('=').Replace('+', '-').Replace('/', '_');
    }

    internal static string NormalizeLabel(string label)
    {
        var trimmed = label.Trim();
        if (trimmed.Length <= MaxLabelLength) return trimmed;
        // Never split a surrogate pair at the limit.
        var cut = char.IsHighSurrogate(trimmed[MaxLabelLength - 1]) ? MaxLabelLength - 1 : MaxLabelLength;
        return trimmed[..cut];
    }

    internal static bool ValidSubject(string? subject) =>
        subject is { Length: 43 } && subject.All(c => char.IsAsciiLetterOrDigit(c) || c is '-' or '_');

    /// <summary>
    /// Reads without the mutex: <c>Revision</c> before and after the other values, again when
    /// they differ (§1). The first reader that finds the key missing creates it with
    /// <c>Version</c> = 1 only, under the mutex, so that a change notification can be armed.
    /// </summary>
    public static DeviceSignInState Read()
    {
        if (!IsCallerEligible) return DeviceSignInState.AbsentState;
        for (var attempt = 0; attempt < 5; attempt++)
        {
            try
            {
                using var root = RegistryKey.OpenBaseKey(RegistryHive.CurrentUser, RegistryView.Registry64);
                using var key = root.OpenSubKey(RegistryKeyPath, writable: false);
                if (key is null)
                {
                    EnsureKey();
                    return DeviceSignInState.AbsentState;
                }
                var before = Revision(key);
                var state = ReadValues(key, before);
                if (Revision(key) == before) return state;
            }
            catch (Exception error) when (error is IOException or UnauthorizedAccessException
                or System.Security.SecurityException or ObjectDisposedException)
            {
                return DeviceSignInState.AbsentState;
            }
        }
        // Writers kept racing this reader; the next trigger reads again.
        return DeviceSignInState.AbsentState;
    }

    /// <summary>
    /// After an interactive sign-in of a person (§2): writes <c>signed_in</c> also when the state
    /// already says the same, so that programs whose silent attempt was refused try again. The
    /// write is conditional: <paramref name="queuedRevision"/> is the revision read when the sign-in
    /// happened (<see cref="Read"/>), and under the mutex the write is dropped when the revision has
    /// moved since (someone wrote later, and that word is newer). Null when the mutex was not had in
    /// 2 s or the caller may not write; the caller tries again at its next trigger with the same
    /// <paramref name="queuedRevision"/>.
    /// </summary>
    public static DeviceSignInWriteResult? WriteSignedIn(string product, DeviceSignInAccount account, ulong queuedRevision) =>
        Locked(current => current.Kind == DeviceSignInKind.NotReadable || current.Revision != queuedRevision
            ? new DeviceSignInWriteResult(false, current)
            : new DeviceSignInWriteResult(true, Write(product, SignedInValue, account, current.Revision)));

    /// <summary>
    /// At launch with a valid own session (§2, §3 "absent"): writes <c>signed_in</c> only when,
    /// under the mutex, the state is still absent. Returns what is there afterwards (the
    /// caller follows it when another program wrote first), or null on a mutex timeout.
    /// </summary>
    public static DeviceSignInState? WriteSignedInIfAbsent(string product, DeviceSignInAccount account) =>
        Locked(current => current.Kind == DeviceSignInKind.Absent
            ? Write(product, SignedInValue, account, current.Revision)
            : current);

    /// <summary>
    /// A person signed out (§2, §5 step 1): one conditional attempt. Dropped (<c>Written</c> false)
    /// when the revision moved since <paramref name="queuedRevision"/>: someone wrote since, and that
    /// word is newer. Null, "not written yet", on a mutex timeout and also while the state is not
    /// readable (a <c>Version</c> other than 1): nothing is written then, and the retry keeps trying
    /// instead of reporting a sign-out the other programs never saw.
    /// </summary>
    public static DeviceSignInWriteResult? WriteSignedOut(string product, ulong queuedRevision) =>
        Locked<DeviceSignInWriteResult>(current => current.Kind == DeviceSignInKind.NotReadable
            ? null
            : current.Revision != queuedRevision
                ? new DeviceSignInWriteResult(false, current)
                : new DeviceSignInWriteResult(true, Write(product, SignedOutValue, null, current.Revision)));

    /// <summary>
    /// A person signed out: retries until <c>signed_out</c> is written, because it is the person's
    /// word (§2), but never past a newer word. <paramref name="queuedRevision"/> is the revision read
    /// when the person signed out; each attempt re-reads the state under the mutex and the pending
    /// write is dropped when someone wrote since (a fresh sign-in elsewhere stays). Null only for a
    /// caller that may not write; only <paramref name="cancellationToken"/> stops the retries.
    /// </summary>
    public static async Task<DeviceSignInWriteResult?> WriteSignedOutUntilWrittenAsync(string product,
        ulong queuedRevision, CancellationToken cancellationToken = default)
    {
        if (!IsCallerEligible) return null;
        var delay = TimeSpan.FromMilliseconds(250);
        while (true)
        {
            var result = await Task.Run(() => WriteSignedOut(product, queuedRevision), cancellationToken)
                .ConfigureAwait(false);
            if (result is not null) return result;
            await Task.Delay(delay, cancellationToken).ConfigureAwait(false);
            delay = TimeSpan.FromTicks(Math.Min(delay.Ticks * 2, TimeSpan.FromSeconds(5).Ticks));
        }
    }

    // ── Internals ──────────────────────────────────────────────────────────

    private static DeviceSignInState ReadValues(RegistryKey key, ulong revision)
    {
        if (key.GetValue("Version") is null || key.GetValueKind("Version") != RegistryValueKind.DWord)
            return DeviceSignInState.AbsentState;
        if ((int)key.GetValue("Version")! != 1) return DeviceSignInState.Unreadable;
        var state = String(key, "State");
        var subject = String(key, "Subject") ?? "";
        var label = String(key, "Label") ?? "";
        var changedBy = String(key, "ChangedBy") ?? "";
        if (revision == 0) return DeviceSignInState.AbsentState;
        return state switch
        {
            SignedInValue when ValidSubject(subject) =>
                new DeviceSignInState(DeviceSignInKind.SignedIn, subject, NormalizeLabel(label), revision, changedBy),
            SignedOutValue => new DeviceSignInState(DeviceSignInKind.SignedOut, "", "", revision, changedBy),
            _ => DeviceSignInState.AbsentState with { Revision = revision },
        };
    }

    private static ulong Revision(RegistryKey key) =>
        key.GetValue("Revision") is long value && key.GetValueKind("Revision") == RegistryValueKind.QWord && value > 0
            ? (ulong)value : 0;

    private static string? String(RegistryKey key, string name) =>
        key.GetValue(name, null, RegistryValueOptions.DoNotExpandEnvironmentNames) is string value
            && key.GetValueKind(name) == RegistryValueKind.String ? value : null;

    private static void EnsureKey() => Locked<DeviceSignInState>(current => current);

    /// <summary>
    /// Holds the per-user mutex (at most 2 s) for the whole read-decide-write. The decision
    /// sees the state as read under the mutex. Null on a timeout or when the mutex or key
    /// cannot be had; nothing is written then.
    /// </summary>
    private static T? Locked<T>(Func<DeviceSignInState, T?> decide) where T : class
    {
        if (!IsCallerEligible || MutexName.Value is not { } name) return null;
        Mutex mutex;
        try
        {
            mutex = new Mutex(false, name);
        }
        catch (Exception error) when (error is UnauthorizedAccessException or IOException or WaitHandleCannotBeOpenedException)
        {
            return null;
        }
        using (mutex)
        {
            var held = false;
            try
            {
                try
                {
                    held = mutex.WaitOne(MutexWait);
                }
                catch (AbandonedMutexException)
                {
                    held = true; // a writer died; Revision is written last, so the state is whole or old
                }
                if (!held) return null;
                using var root = RegistryKey.OpenBaseKey(RegistryHive.CurrentUser, RegistryView.Registry64);
                using var key = root.OpenSubKey(RegistryKeyPath, writable: true)
                    ?? root.CreateSubKey(RegistryKeyPath, writable: true);
                if (key.GetValue("Version") is null) key.SetValue("Version", 1, RegistryValueKind.DWord);
                var current = ReadValues(key, Revision(key));
                current = current with { Revision = Revision(key) };
                Pending = key;
                try
                {
                    return decide(current);
                }
                finally
                {
                    Pending = null;
                }
            }
            catch (Exception error) when (error is IOException or UnauthorizedAccessException
                or System.Security.SecurityException or ObjectDisposedException)
            {
                return null;
            }
            finally
            {
                if (held) mutex.ReleaseMutex();
            }
        }
    }

    // The key opened by Locked for its decision; only Write uses it, on the same thread.
    [ThreadStatic] private static RegistryKey? Pending;

    private static DeviceSignInState Write(string product, string state, DeviceSignInAccount? account, ulong previous)
    {
        if (!DeviceSignInProduct.Valid(product)) throw new ArgumentException("Unknown product.", nameof(product));
        if (state == SignedInValue && (account is null || !ValidSubject(account.Subject)))
            throw new ArgumentException("A signed-in state needs an account subject hash.", nameof(account));
        var key = Pending ?? throw new InvalidOperationException("Writes happen only under the mutex.");
        var subject = state == SignedInValue ? account!.Subject : "";
        var label = state == SignedInValue ? NormalizeLabel(account!.Label) : "";
        var revision = previous + 1;
        key.SetValue("Version", 1, RegistryValueKind.DWord);
        key.SetValue("State", state, RegistryValueKind.String);
        key.SetValue("Subject", subject, RegistryValueKind.String);
        key.SetValue("Label", label, RegistryValueKind.String);
        key.SetValue("ChangedAt", DateTime.UtcNow.ToString("yyyy-MM-dd'T'HH:mm:ss'Z'", CultureInfo.InvariantCulture),
            RegistryValueKind.String);
        key.SetValue("ChangedBy", product, RegistryValueKind.String);
        key.SetValue("Revision", unchecked((long)revision), RegistryValueKind.QWord); // last
        return new DeviceSignInState(state == SignedInValue ? DeviceSignInKind.SignedIn : DeviceSignInKind.SignedOut,
            subject, label, revision, product);
    }
}

/// <summary>What one loopback request of a sign-in carried: a ticket or code, or a refusal.</summary>
public sealed record DeviceSignInCallback(string? Ticket, string? Code, string? Error);

/// <summary>The redirect and the state of one silent request, set in <see cref="DeviceSignInSilentRoute"/> only.</summary>
public sealed record DeviceSignInSilentRequest(string RedirectUri, string State);

/// <summary>
/// The one place that sets a silent sign-in's loopback redirect and state (§4) and reads the
/// loopback request that answers it. It carries no relay: the kombify HTTPS relay
/// (kombify-Gateway) serves programs whose own authorization request has a loopback redirect,
/// and neither client here is one of them (coordinator 62ca, 2026-11-15). Techstack's sign-in
/// runs through its own server's HTTPS callback; the program names its loopback and state and the
/// server's handoff returns there (StackKits #3407). Paperwork's system-browser path is off.
/// </summary>
public static class DeviceSignInSilentRoute
{
    public const string CallbackPath = "/oauth/callback";
    public const string TechstackWindows = "techstack-windows";
    public const string PaperworkWindows = "paperwork-windows";

    /// <summary>
    /// The loopback range of each client (contract table, kombify-workspace #5294; coordinator 62ca,
    /// 2026-11-15): <c>paperwork-windows</c> 63700-63704 (its interactive sign-in today and its silent
    /// path later), <c>techstack-windows</c> 63720-63724 (reserved). The Companion has 63710-63714 and
    /// Workbench 63684-63688. Techstack's existing login through its own server keeps 63690-63694,
    /// which is not a range of this table. The relay refuses both clients until those paths exist.
    /// </summary>
    public static (int First, int Last)? Ports(string client) => client switch
    {
        PaperworkWindows => (63700, 63704),
        TechstackWindows => (63720, 63724),
        _ => null,
    };

    /// <summary>
    /// The silent request of <paramref name="client"/> listening on <paramref name="port"/>, for a
    /// client whose own authorization request carries the redirect: the loopback and a state
    /// <c>&lt;client&gt;.&lt;port&gt;.&lt;43 random base64url characters&gt;</c>. A client without a
    /// registered range, or a port outside it, is refused.
    /// </summary>
    public static DeviceSignInSilentRequest Request(string client, int port)
    {
        if (Ports(client) is not { } range || port < range.First || port > range.Last)
            throw new ArgumentException("The client has no registered loopback range for this port.", nameof(port));
        return new DeviceSignInSilentRequest(LoopbackUri(port), State(client, port));
    }

    /// <summary>
    /// The attempt state <c>&lt;client&gt;.&lt;port&gt;.&lt;43 random base64url characters&gt;</c>, the one
    /// format of every loopback return. Format only: the caller owns the port's range. Techstack's
    /// login through its own server uses it with its 63690-63694 (StackKits #3407 checks the
    /// format and the range on <c>native/start</c> and <c>native/redeem</c>).
    /// </summary>
    public static string State(string client, int port)
    {
        if (client is not (TechstackWindows or PaperworkWindows))
            throw new ArgumentException("Unknown client.", nameof(client));
        var nonce = Convert.ToBase64String(RandomNumberGenerator.GetBytes(32))
            .TrimEnd('=').Replace('+', '-').Replace('/', '_');
        return client + "." + port.ToString(CultureInfo.InvariantCulture) + "." + nonce;
    }

    public static string LoopbackUri(int port) =>
        "http://127.0.0.1:" + port.ToString(CultureInfo.InvariantCulture) + CallbackPath;

    /// <summary>
    /// Reads one request on the loopback listener of <paramref name="port"/>: <c>GET /oauth/callback?...</c>
    /// with the exact <c>Host</c> and <paramref name="expectedState"/>, carrying a ticket, a code or an
    /// error. Null for anything else.
    /// </summary>
    public static DeviceSignInCallback? ReadCallback(string requestLine, string? host, int port, string expectedState)
    {
        var expectedHost = "127.0.0.1:" + port.ToString(CultureInfo.InvariantCulture);
        var parts = requestLine.Split(' ', 3);
        if (parts.Length != 3 || parts[0] != "GET" || host != expectedHost || parts[1].Length >= 2048
            || !Uri.TryCreate("http://" + expectedHost + parts[1], UriKind.Absolute, out var callback)
            || callback.AbsolutePath != CallbackPath)
            return null;
        var query = System.Web.HttpUtility.ParseQueryString(callback.Query);
        if (string.IsNullOrEmpty(expectedState) || query.Get("state") != expectedState) return null;
        static string? Value(string? v) => string.IsNullOrEmpty(v) ? null : v;
        var result = new DeviceSignInCallback(Value(query.Get("ticket")), Value(query.Get("code")), Value(query.Get("error")));
        return result.Ticket is null && result.Code is null && result.Error is null ? null : result;
    }
}

/// <summary>
/// Raises <see cref="Changed"/> (on a background thread) whenever a value under the shared key
/// changes, through <c>RegNotifyChangeKeyValue</c> re-armed after each notification (§3).
/// </summary>
public sealed class DeviceSignInWatcher : IDisposable
{
    private readonly ManualResetEvent _stop = new(false);
    private readonly Thread? _thread;

    public event Action? Changed;

    public DeviceSignInWatcher()
    {
        if (!DeviceSignIn.IsCallerEligible) return;
        _thread = new Thread(Run) { IsBackground = true, Name = "kombify device sign-in watcher" };
        _thread.Start();
    }

    private void Run()
    {
        var backoff = TimeSpan.FromSeconds(1);
        while (!_stop.WaitOne(0))
        {
            try
            {
                using var root = RegistryKey.OpenBaseKey(RegistryHive.CurrentUser, RegistryView.Registry64);
                var key = root.OpenSubKey(RegistryKeyPath, RegistryKeyPermissionCheck.ReadSubTree,
                    System.Security.AccessControl.RegistryRights.Notify | System.Security.AccessControl.RegistryRights.ReadKey);
                if (key is null)
                {
                    _ = DeviceSignIn.Read(); // creates the key so that it can be watched
                    if (_stop.WaitOne(backoff)) return;
                    continue;
                }
                using (key)
                using (var changed = new AutoResetEvent(false))
                {
                    while (!_stop.WaitOne(0))
                    {
                        var status = DeviceSignInNative.RegNotifyChangeKeyValue(key.Handle,
                            false, DeviceSignInNative.NotifyLastSet | DeviceSignInNative.NotifyName,
                            changed.SafeWaitHandle, true);
                        if (status != 0) throw new Win32Exception(status);
                        if (WaitHandle.WaitAny([_stop, changed]) == 0) return;
                        backoff = TimeSpan.FromSeconds(1);
                        Changed?.Invoke();
                        // A deleted key signals too: reopen it.
                        using var probe = root.OpenSubKey(RegistryKeyPath, writable: false);
                        if (probe is null) break;
                    }
                }
            }
            catch (Exception error) when (error is IOException or UnauthorizedAccessException
                or System.Security.SecurityException or ObjectDisposedException or Win32Exception)
            {
                if (_stop.WaitOne(backoff)) return;
                backoff = TimeSpan.FromTicks(Math.Min(backoff.Ticks * 2, TimeSpan.FromSeconds(30).Ticks));
            }
        }
    }

    private const string RegistryKeyPath = DeviceSignIn.RegistryKeyPath;

    public void Dispose()
    {
        // The event is not disposed: a thread still inside a registry call reads it once more.
        _stop.Set();
        _thread?.Join(TimeSpan.FromSeconds(2));
    }
}

/// <summary>What a program wires to its own session; the follower decides when each is called.</summary>
public interface IDeviceSignInSession
{
    /// <summary><c>ChangedBy</c> of this program.</summary>
    string Product { get; }

    /// <summary>The account of the program's own valid session, or null when it has none.</summary>
    Task<DeviceSignInAccount?> CurrentAccountAsync(CancellationToken cancellationToken);

    /// <summary>
    /// Ends the program's own session (§5): revokes its own refresh token, deletes its own
    /// credentials and cookies and shows its ordinary signed-out state. Never writes the state,
    /// never shows an error.
    /// </summary>
    Task EndSessionAsync(CancellationToken cancellationToken);

    /// <summary>
    /// One silent sign-in (§4): its own request in the system browser with <c>prompt=none</c>.
    /// Returns the account it now holds, or null when refused (it then shows its ordinary
    /// signed-out state, no error). A program should not show a session whose subject is not
    /// <paramref name="expectedSubject"/>; the follower discards and revokes it either way.
    /// <paramref name="cancellationToken"/> ends at 60 s.
    /// </summary>
    Task<DeviceSignInAccount?> SilentSignInAsync(string expectedSubject, CancellationToken cancellationToken);
}

/// <summary>
/// The follow table of §3 as a small state machine. Call <see cref="Start"/> once on the UI
/// thread after the program's first screen is ready (the launch read), <see cref="OnActivated"/>
/// from window activation, <see cref="SignedInInteractivelyAsync"/> after the person's own
/// sign-in and <see cref="SignedOutByPersonAsync"/> first when the person signs out. It acts
/// at most once per revision in this process run, runs one step at a time, never loops and
/// never throws into the caller; everything is a no-op for an elevated process.
/// </summary>
public sealed class DeviceSignInFollower : IDisposable
{
    public static readonly TimeSpan SilentAttemptLimit = TimeSpan.FromSeconds(60);

    private readonly IDeviceSignInSession _session;
    private readonly SemaphoreSlim _gate = new(1, 1);
    private readonly CancellationTokenSource _closed = new();
    private DeviceSignInWatcher? _watcher;
    private SynchronizationContext? _context;
    private readonly object _sync = new();
    private ulong _actedRevision; // this process run only; never persisted

    private ulong ActedRevision
    {
        get { lock (_sync) return _actedRevision; }
    }

    private void ActedOn(ulong revision)
    {
        lock (_sync)
        {
            if (revision > _actedRevision) _actedRevision = revision;
        }
    }

    public DeviceSignInFollower(IDeviceSignInSession session)
    {
        if (!DeviceSignInProduct.Valid(session.Product)) throw new ArgumentException("Unknown product.", nameof(session));
        _session = session;
    }

    /// <summary>True for a process that takes part (not elevated, not AppContainer).</summary>
    public static bool Enabled => DeviceSignIn.IsCallerEligible;

    /// <summary>The last state read; the program may show its label.</summary>
    public DeviceSignInState? Last { get; private set; }

    /// <summary>Launch read, then watches the key. Captures the caller's context for the follow steps.</summary>
    public Task StartAsync()
    {
        if (!Enabled) return Task.CompletedTask;
        if (_watcher is not null) return FollowAsync();
        _context = SynchronizationContext.Current;
        _watcher = new DeviceSignInWatcher();
        _watcher.Changed += OnChanged;
        return FollowAsync();
    }

    public void OnActivated()
    {
        if (Enabled && _watcher is not null) _ = FollowAsync();
    }

    private void OnChanged()
    {
        if (_context is null) _ = FollowAsync();
        else _context.Post(_ => _ = FollowAsync(), null);
    }

    /// <summary>
    /// After an interactive sign-in of the person in this program (§2): writes <c>signed_in</c>
    /// (raising the revision) and remembers that revision as acted on. A mutex timeout is
    /// retried at the next trigger.
    /// </summary>
    public async Task SignedInInteractivelyAsync(DeviceSignInAccount account)
    {
        if (!Enabled) return;
        // The revision the sign-in happened at: a retried write never passes a newer word.
        var queued = await Task.Run(DeviceSignIn.Read).ConfigureAwait(true);
        _pendingInteractive = (account, queued.Revision);
        await FollowAsync().ConfigureAwait(true);
    }

    private (DeviceSignInAccount Account, ulong Revision)? _pendingInteractive;

    /// <summary>
    /// The person signed out in this program (§5 step 1): starts writing <c>signed_out</c>, retried
    /// until written, and returns once it is written or after <paramref name="waitAtMost"/>
    /// (default 5 s), so that the program's own sign-out never hangs on a held mutex. The retry
    /// continues in the background until written or the follower is disposed. True when written.
    /// </summary>
    public async Task<bool> SignedOutByPersonAsync(TimeSpan? waitAtMost = null)
    {
        if (!Enabled) return false;
        _pendingInteractive = null;
        var queued = await Task.Run(DeviceSignIn.Read).ConfigureAwait(true);
        var write = WriteSignedOutAsync(queued.Revision);
        try
        {
            await write.WaitAsync(waitAtMost ?? TimeSpan.FromSeconds(5)).ConfigureAwait(true);
            return true;
        }
        catch (TimeoutException)
        {
            return false;
        }
    }

    private async Task WriteSignedOutAsync(ulong queuedRevision)
    {
        try
        {
            var result = await DeviceSignIn.WriteSignedOutUntilWrittenAsync(_session.Product, queuedRevision, _closed.Token)
                .ConfigureAwait(false);
            if (result is null) return;
            Last = result.State;
            // Written: this program's own revision. Dropped: someone wrote since; the follower
            // follows that newer word at its next step.
            if (result.Written) ActedOn(result.State.Revision);
        }
        catch (OperationCanceledException)
        {
            // The program is closing.
        }
    }

    /// <summary>One follow step; concurrent triggers queue behind it and find nothing new.</summary>
    public async Task FollowAsync()
    {
        if (!Enabled || _closed.IsCancellationRequested) return;
        try
        {
            await _gate.WaitAsync(_closed.Token).ConfigureAwait(true);
        }
        catch (OperationCanceledException)
        {
            return;
        }
        var attempted = false;
        try
        {
            attempted = await StepAsync(_closed.Token).ConfigureAwait(true);
        }
        catch (Exception error) when (error is not OutOfMemoryException)
        {
            // A follow step never surfaces an error; the next trigger reads again.
        }
        finally
        {
            _gate.Release();
        }
        // A silent attempt can wait up to 60 s. A change that landed meanwhile is never lost:
        // its notification queues a step behind the gate, and independently of that the state
        // is read again once the attempt ended and followed when Revision moved past the one
        // acted on. A session the attempt adopted while the state moved is ended then (§5).
        if (attempted && !_closed.IsCancellationRequested)
        {
            var now = await Task.Run(DeviceSignIn.Read).ConfigureAwait(true);
            if (now.Kind != DeviceSignInKind.NotReadable && now.Revision > ActedRevision) await FollowAsync().ConfigureAwait(true);
        }
    }

    /// <returns>True when the step ran a silent attempt.</returns>
    private async Task<bool> StepAsync(CancellationToken token)
    {
        if (_pendingInteractive is { } pending)
        {
            var result = await Task.Run(() => DeviceSignIn.WriteSignedIn(_session.Product, pending.Account, pending.Revision),
                token).ConfigureAwait(true);
            if (result is null) return false; // mutex timeout: retried at the next trigger, same queued revision
            _pendingInteractive = null;
            Last = result.State;
            if (result.Written)
            {
                ActedOn(result.State.Revision);
                return false;
            }
            if (result.State.Kind == DeviceSignInKind.NotReadable) return false;
            // Dropped: someone wrote after this sign-in; follow that newer state below.
        }

        var state = await Task.Run(DeviceSignIn.Read, token).ConfigureAwait(true);
        Last = state;
        if (state.Kind == DeviceSignInKind.NotReadable) return false;
        if (state.Kind != DeviceSignInKind.Absent && state.Revision <= ActedRevision) return false;

        var current = await _session.CurrentAccountAsync(token).ConfigureAwait(true);
        if (state.Kind == DeviceSignInKind.Absent)
        {
            if (current is null) return false;
            var after = await Task.Run(() => DeviceSignIn.WriteSignedInIfAbsent(_session.Product, current), token)
                .ConfigureAwait(true);
            if (after is null) return false; // mutex timeout: the next trigger tries again
            Last = after;
            if (after.Kind is DeviceSignInKind.NotReadable or DeviceSignInKind.Absent) return false;
            if (after.Kind == DeviceSignInKind.SignedIn && after.Subject == current.Subject)
            {
                ActedOn(after.Revision);
                return false;
            }
            state = after; // another program wrote first: follow what it wrote
            if (state.Revision <= ActedRevision) return false;
        }

        ActedOn(state.Revision); // at most once per revision, before acting
        switch (state.Kind)
        {
            case DeviceSignInKind.SignedOut:
                if (current is not null) await _session.EndSessionAsync(token).ConfigureAwait(true);
                return false;
            case DeviceSignInKind.SignedIn when current is not null && current.Subject == state.Subject:
                return false;
            case DeviceSignInKind.SignedIn:
                if (current is not null) await _session.EndSessionAsync(token).ConfigureAwait(true);
                await SilentAsync(state, token).ConfigureAwait(true);
                return true;
        }
        return false;
    }

    private async Task SilentAsync(DeviceSignInState state, CancellationToken token)
    {
        using var limit = CancellationTokenSource.CreateLinkedTokenSource(token);
        limit.CancelAfter(SilentAttemptLimit);
        DeviceSignInAccount? obtained;
        try
        {
            obtained = await _session.SilentSignInAsync(state.Subject, limit.Token).ConfigureAwait(true);
        }
        catch (OperationCanceledException) when (!token.IsCancellationRequested)
        {
            obtained = null; // 60 s: refused
        }
        if (obtained is not null && obtained.Subject != state.Subject)
        {
            // Tokens of another account (§4): discarded and revoked; the attempt counts as refused.
            await _session.EndSessionAsync(token).ConfigureAwait(true);
        }
    }

    public void Dispose()
    {
        _closed.Cancel();
        if (_watcher is not null)
        {
            _watcher.Changed -= OnChanged;
            _watcher.Dispose();
        }
    }
}

internal static class DeviceSignInNative
{
    public const int NotifyName = 0x00000001;
    public const int NotifyLastSet = 0x00000004;

    [DllImport("advapi32.dll")]
    public static extern int RegNotifyChangeKeyValue(SafeRegistryHandle key,
        [MarshalAs(UnmanagedType.Bool)] bool watchSubtree, int notifyFilter, SafeWaitHandle eventHandle,
        [MarshalAs(UnmanagedType.Bool)] bool asynchronous);
}
