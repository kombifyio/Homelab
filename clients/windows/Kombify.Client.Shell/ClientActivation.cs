using System.IO.Pipes;
using System.Runtime.InteropServices;
using System.Security.AccessControl;
using System.Security.Principal;
using System.Text;
using System.Text.Json;

namespace Kombify.Client.Shell;

/// <summary>
/// URL activation into a running client (AppLink v2 "Settings from Kubi's right-click"): when Windows
/// starts a second instance for <c>&lt;scheme&gt;://open/?route=…</c>, that instance
/// hands the validated route to the running one and exits.
///
/// Mechanism: a named pipe per client scheme, Windows user and logon session,
/// <c>\\.\pipe\kombify.activation.&lt;scheme&gt;.&lt;SID&gt;.&lt;session&gt;</c>, created by
/// the running instance as the first pipe instance, with a protected DACL that
/// grants only the current user and Windows' default medium mandatory label (no
/// low-integrity writer), local clients only. Both ends prove the other by OS
/// evidence before a byte moves: same SID and session, medium integrity, not
/// AppContainer, and the same executable image as this process. The one message is
/// <c>{"version":1}</c> or <c>{"version":1,"route":"/…"}</c>: only a route that
/// <see cref="CompanionOpenInApp.TryParse"/> produced (sender) and
/// <see cref="CompanionOpenInApp"/>'s route rules admit again (receiver) ever
/// crosses it; any other frame is refused without an answer. The sender lets the
/// running instance take the foreground (<c>AllowSetForegroundWindow</c>).
///
/// Use with the client's existing single-instance mutex: the first instance calls
/// <see cref="Listen"/>; a later one calls <see cref="HandOffAsync"/> with
/// <see cref="RouteFromArguments"/> and exits when it returns true.
/// </summary>
public sealed class ClientActivation : IDisposable
{
    public const int ProtocolVersion = 1;
    private const int MaxFrameBytes = 1024;
    private static readonly TimeSpan Deadline = TimeSpan.FromSeconds(2);
    private static readonly byte[] Accepted = Encoding.ASCII.GetBytes("{\"accepted\":true}");

    private readonly string _name;
    private readonly CompanionAppLink.Peer _self;
    private readonly Action<string?> _activated;
    private readonly CancellationTokenSource _stop = new();

    private ClientActivation(string name, CompanionAppLink.Peer self, Action<string?> activated)
    {
        _name = name;
        _self = self;
        _activated = activated;
    }

    /// <summary>
    /// The route of an activation URI among <paramref name="arguments"/> (Windows passes it as
    /// the one argument of <c>"exe" "%1"</c>), or null when none parses through
    /// <see cref="CompanionOpenInApp.TryParse"/>. Null means "come to the front", nothing more.
    /// </summary>
    public static string? RouteFromArguments(IReadOnlyList<string> arguments, string scheme)
    {
        foreach (var argument in arguments)
            if (CompanionOpenInApp.TryParse(argument, scheme, out var route)) return route;
        return null;
    }

    /// <summary>
    /// Starts receiving hand-overs for this client. <paramref name="activated"/> gets the route
    /// (null: just come to the front) on a thread-pool thread; marshal it to the UI thread and
    /// navigate inside the existing profile, nothing else. Throws <c>endpoint_conflict</c> when
    /// another process already holds the name, <c>invalid_request</c> for a scheme that is not
    /// this product's, and refuses an elevated or low-integrity caller.
    /// </summary>
    public static ClientActivation Listen(string product, string scheme, Action<string?> activated)
    {
        CompanionAppLink.RequireEligibleCaller();
        if (!CompanionOpenInApp.ValidScheme(product, scheme)) throw CompanionAppLink.Fail(CompanionAppLinkError.InvalidRequest);
        var self = CompanionAppLink.LocalPeer();
        var name = PipeName(scheme, self);
        NamedPipeServerStream first;
        try
        {
            first = Server(name, self.Sid, firstInstance: true);
        }
        catch (Exception error) when (error is IOException or UnauthorizedAccessException)
        {
            throw CompanionAppLink.Fail(CompanionAppLinkError.EndpointConflict);
        }
        var listener = new ClientActivation(name, self, activated);
        _ = listener.RunAsync(first);
        return listener;
    }

    /// <summary>
    /// Hands <paramref name="route"/> (null: just come to the front) to the running instance of
    /// this client. True only when that instance, proven to be this same executable of this
    /// user and session, accepted it. Waits at most two seconds for a listener that is still
    /// starting. Never throws, except for the caller's own cancellation.
    /// </summary>
    public static async Task<bool> HandOffAsync(string product, string scheme, string? route,
        CancellationToken cancellationToken = default)
    {
        try
        {
            CompanionAppLink.RequireEligibleCaller();
            if (!CompanionOpenInApp.ValidScheme(product, scheme) || (route is not null && !CompanionAppLink.ValidRoute(route)))
                return false;
            var self = CompanionAppLink.LocalPeer();
            using var deadline = CancellationTokenSource.CreateLinkedTokenSource(cancellationToken);
            deadline.CancelAfter(Deadline);
            NamedPipeClientStream pipe;
            uint serverPid;
            while (true)
            {
                try
                {
                    (pipe, serverPid) = await CompanionAppLink.DialAsync(@"\\.\pipe\" + PipeName(scheme, self),
                        (me, server) => SameClient(me, server), deadline.Token).ConfigureAwait(false);
                    break;
                }
                catch (CompanionAppLinkException error) when (error.Code == CompanionAppLinkError.NotRunning)
                {
                    await Task.Delay(100, deadline.Token).ConfigureAwait(false); // the first instance is still starting
                }
            }
            using (pipe)
            {
                _ = ActivationNative.AllowSetForegroundWindow(serverPid);
                var answer = await CompanionAppLink.ExchangeAsync(pipe, Request(route), MaxFrameBytes, deadline.Token,
                    cancellationToken).ConfigureAwait(false);
                return answer.AsSpan().SequenceEqual(Accepted);
            }
        }
        catch (OperationCanceledException) when (cancellationToken.IsCancellationRequested)
        {
            throw;
        }
        catch (Exception error) when (error is not OutOfMemoryException)
        {
            return false;
        }
    }

    public void Dispose() => _stop.Cancel();

    private static string PipeName(string scheme, CompanionAppLink.Peer self) =>
        $"kombify.activation.{scheme}.{self.Sid}.{self.Session}";

    private static bool SameClient(CompanionAppLink.Peer me, CompanionAppLink.Peer other) =>
        other.Sid == me.Sid && other.Session == me.Session && CompanionAppLink.Eligibility(other) is null
        && string.Equals(other.Executable, me.Executable, StringComparison.OrdinalIgnoreCase);

    private static NamedPipeServerStream Server(string name, string sid, bool firstInstance)
    {
        var security = new PipeSecurity();
        security.SetAccessRuleProtection(isProtected: true, preserveInheritance: false);
        security.AddAccessRule(new PipeAccessRule(new SecurityIdentifier(sid), PipeAccessRights.FullControl,
            AccessControlType.Allow));
        var options = PipeOptions.Asynchronous | (firstInstance ? PipeOptions.FirstPipeInstance : PipeOptions.None);
        // .NET opens every server with PIPE_REJECT_REMOTE_CLIENTS. No instance limit: another same-user process
        // creating instances cannot use up the slots of this listener (a client still admits only this executable).
        return NamedPipeServerStreamAcl.Create(name, PipeDirection.InOut,
            NamedPipeServerStream.MaxAllowedServerInstances, PipeTransmissionMode.Byte, options,
            MaxFrameBytes, MaxFrameBytes, security);
    }

    private async Task RunAsync(NamedPipeServerStream first)
    {
        var token = _stop.Token;
        NamedPipeServerStream? current = first;
        try
        {
            while (!token.IsCancellationRequested)
            {
                try
                {
                    // After a failure the listener comes back with a new instance; only Dispose ends it.
                    current ??= Server(_name, _self.Sid, firstInstance: false);
                    await current.WaitForConnectionAsync(token).ConfigureAwait(false);
                    // The next instance exists before this one closes, so the name stays held while one is served.
                    NamedPipeServerStream? next = null;
                    try { next = Server(_name, _self.Sid, firstInstance: false); }
                    catch (Exception error) when (error is IOException or UnauthorizedAccessException) { }
                    var served = current;
                    current = next;
                    using (served) await ServeAsync(served, token).ConfigureAwait(false);
                }
                catch (OperationCanceledException) when (token.IsCancellationRequested)
                {
                    break;
                }
                catch (Exception error) when (error is IOException or UnauthorizedAccessException or ObjectDisposedException)
                {
                    // A broken instance, or no instance could be made (another same-user process holds
                    // the name): drop it and try again after a second.
                    current?.Dispose();
                    current = null;
                    await Task.Delay(TimeSpan.FromSeconds(1), token).ConfigureAwait(false);
                }
            }
        }
        catch (OperationCanceledException)
        {
            // Stopped.
        }
        finally
        {
            current?.Dispose();
        }
    }

    private async Task ServeAsync(NamedPipeServerStream pipe, CancellationToken stop)
    {
        try
        {
            if (!ActivationNative.GetNamedPipeClientProcessId(pipe.SafePipeHandle, out var clientPid)
                || !SameClient(_self, CompanionAppLink.ProcessPeer(clientPid)))
                return; // closed without an answer
            using var deadline = CancellationTokenSource.CreateLinkedTokenSource(stop);
            deadline.CancelAfter(Deadline);
            var frame = await CompanionAppLink.ReadFrameAsync(pipe, MaxFrameBytes, deadline.Token).ConfigureAwait(false);
            if (!TryParse(frame, out var route)) return;
            try { _activated(route); }
            catch (Exception error) when (error is not OutOfMemoryException) { return; }
            var answer = new byte[Accepted.Length + 1];
            Accepted.CopyTo(answer, 0);
            answer[^1] = (byte)'\n';
            await pipe.WriteAsync(answer, deadline.Token).ConfigureAwait(false);
            await pipe.FlushAsync(deadline.Token).ConfigureAwait(false);
        }
        catch (Exception error) when (error is not OutOfMemoryException)
        {
            // A refused peer, a malformed or late frame: this connection ends, the listener goes on.
        }
    }

    private static byte[] Request(string? route)
    {
        var output = new System.Buffers.ArrayBufferWriter<byte>();
        using (var w = new Utf8JsonWriter(output))
        {
            w.WriteStartObject();
            w.WriteNumber("version", ProtocolVersion);
            if (route is not null) w.WriteString("route", route);
            w.WriteEndObject();
        }
        return output.WrittenSpan.ToArray();
    }

    /// <summary>Exactly <c>{"version":1}</c> or <c>{"version":1,"route":"…"}</c> with a valid route; nothing else.</summary>
    private static bool TryParse(byte[] frame, out string? route)
    {
        route = null;
        try
        {
            var reader = new Utf8JsonReader(frame, new JsonReaderOptions { MaxDepth = 2 });
            if (!reader.Read() || reader.TokenType != JsonTokenType.StartObject) return false;
            var version = false;
            var seenRoute = false;
            while (reader.Read() && reader.TokenType == JsonTokenType.PropertyName)
            {
                var name = reader.GetString();
                if (!reader.Read()) return false;
                if (name == "version" && !version && reader.TokenType == JsonTokenType.Number
                    && reader.ValueSpan.SequenceEqual("1"u8))
                {
                    version = true;
                }
                else if (name == "route" && !seenRoute && reader.TokenType == JsonTokenType.String)
                {
                    seenRoute = true;
                    route = reader.GetString();
                    if (!CompanionAppLink.ValidRoute(route)) return false;
                }
                else
                {
                    return false;
                }
            }
            if (reader.TokenType != JsonTokenType.EndObject || reader.Read() || !version) return false;
            return true;
        }
        catch (JsonException)
        {
            return false;
        }
    }
}

internal static class ActivationNative
{
    [DllImport("kernel32.dll", SetLastError = true)]
    [return: MarshalAs(UnmanagedType.Bool)]
    public static extern bool GetNamedPipeClientProcessId(Microsoft.Win32.SafeHandles.SafePipeHandle pipe, out uint clientProcessId);

    [DllImport("user32.dll", SetLastError = true)]
    [return: MarshalAs(UnmanagedType.Bool)]
    public static extern bool AllowSetForegroundWindow(uint processId);
}
