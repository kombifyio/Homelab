using System.Text.Json;
using System.Text.Json.Serialization;
using Kombify.Client.Shell;
using Microsoft.Web.WebView2.Core;

namespace Kombify.Homelab.Client;

// Settings › AI providers and the local answer: the device-wide local AI runtimes (workspace
// platform/standards/local-ai-runtimes.v1.md) through Kombify.Client.Shell LocalAiRuntimes. The page
// talks to this window through fixed, typed messages, gated like the device-context read (the
// current document of the product's own authority, its revision and token); answers go to that
// document only. The shell probes and talks to the runtime itself, so the page needs no access to it.
//
//   read        the file, with the status of each switched-on runtime and of the answer choice
//   write       one runtime's switch or server address
//   add         a runtime on another machine (the Ollama of a homelab server), switched on
//   remove      a runtime on another machine
//   answer      the answer choice of rule 9 (a runtime row and model), or none
//   chat        one conversation turn on the answer choice's runtime: `chat-delta` pieces, then the result
//   chat-cancel stops a running turn
//
// The page never names the runtime a turn runs on: the shell takes it from the file, so a turn runs
// only where the person chose. A change of the file by any app sends `changed` (no data) to the
// current document, which reads again.
internal sealed partial class ClientWindow
{
    private const string LocalAiPrefix = "kombify.homelab.local-ai.";
    private const string LocalAiResult = LocalAiPrefix + "result.v1";
    private const string LocalAiChanged = LocalAiPrefix + "changed.v1";
    private const string LocalAiChatDelta = LocalAiPrefix + "chat-delta.v1";
    private const int LocalAiMaxMessageChars = 4096;
    // A conversation with its history and the page it is about.
    private const int LocalAiMaxChatChars = 96 * 1024;
    private const int LocalAiMaxChatMessages = 64;

    private sealed record LocalAiRequest(string Type, string RequestId, long DocumentRevision, string DocumentToken);
    private sealed record LocalAiWriteRequest(string Type, string RequestId, long DocumentRevision, string DocumentToken,
        string Runtime, bool? Enabled = null, string? Endpoint = null);
    private sealed record LocalAiAddRequest(string Type, string RequestId, long DocumentRevision, string DocumentToken,
        string Label, string Endpoint);
    private sealed record LocalAiRemoveRequest(string Type, string RequestId, long DocumentRevision, string DocumentToken,
        string Runtime);
    private sealed record LocalAiAnswerRequest(string Type, string RequestId, long DocumentRevision, string DocumentToken,
        string? Runtime = null, string? Model = null);
    private sealed record LocalAiChatMessageDto(string Role, string Content);
    private sealed record LocalAiChatRequest(string Type, string RequestId, long DocumentRevision, string DocumentToken,
        LocalAiChatMessageDto[] Messages);

    private static readonly JsonSerializerOptions LocalAiJson = new()
    {
        PropertyNamingPolicy = JsonNamingPolicy.SnakeCaseLower,
        UnmappedMemberHandling = JsonUnmappedMemberHandling.Disallow,
        RespectRequiredConstructorParameters = true,
    };

    private IDisposable? _localAiWatch;
    private readonly Dictionary<string, CancellationTokenSource> _localAiChats = new(StringComparer.Ordinal);

    private void RegisterLocalAiBridge()
    {
        _webView.CoreWebView2.WebMessageReceived += OnLocalAiRequested;
        try { _localAiWatch = LocalAiRuntimes.Watch(() => BeginInvoke(new Action(NotifyLocalAiChanged))); }
        catch (Exception error) when (error is IOException or UnauthorizedAccessException or ArgumentException) { /* the page still reads on open */ }
        FormClosed += (_, _) =>
        {
            _localAiWatch?.Dispose();
            foreach (var chat in _localAiChats.Values) chat.Cancel();
        };
    }

    private void NotifyLocalAiChanged()
    {
        if (IsDisposed || _webView.CoreWebView2 is null) return;
        if (!IsCurrentDeviceContextDocument(_webView.CoreWebView2.Source, _deviceContextDocumentRevision, _deviceContextDocumentToken)) return;
        PostLocalAi(new Dictionary<string, object?> { ["type"] = LocalAiChanged, ["document_revision"] = _deviceContextDocumentRevision });
    }

    private void OnLocalAiRequested(object? sender, CoreWebView2WebMessageReceivedEventArgs args)
    {
        var source = args.Source;
        var revision = _deviceContextDocumentRevision;
        var token = _deviceContextDocumentToken;
        if (!IsCurrentDeviceContextDocument(source, revision, token)) return;
        try
        {
            var json = args.WebMessageAsJson;
            if (json.Length > LocalAiMaxChatChars) return;
            using var peek = JsonDocument.Parse(json);
            if (peek.RootElement.ValueKind != JsonValueKind.Object
                || !peek.RootElement.TryGetProperty("type", out var typeElement)
                || typeElement.ValueKind != JsonValueKind.String) return;
            var type = typeElement.GetString();
            if (type != LocalAiPrefix + "chat.v1" && json.Length > LocalAiMaxMessageChars) return;
            bool Bound(string requestId, long documentRevision, string documentToken) =>
                Guid.TryParse(requestId, out _) && documentRevision == revision && documentToken == token;
            switch (type)
            {
                case LocalAiPrefix + "read.v1":
                    if (JsonSerializer.Deserialize<LocalAiRequest>(json, LocalAiJson) is { } read
                        && Bound(read.RequestId, read.DocumentRevision, read.DocumentToken))
                        _ = LocalAiReplyAsync(source, revision, token, read.RequestId, "read", LocalAiRuntimes.Read());
                    break;
                case LocalAiPrefix + "write.v1":
                    if (JsonSerializer.Deserialize<LocalAiWriteRequest>(json, LocalAiJson) is { } write
                        && Bound(write.RequestId, write.DocumentRevision, write.DocumentToken))
                        LocalAiChange(source, revision, token, write.RequestId, "write",
                            () => LocalAiRuntimes.Set(write.Runtime, write.Enabled, write.Endpoint, "homelab"));
                    break;
                case LocalAiPrefix + "add.v1":
                    if (JsonSerializer.Deserialize<LocalAiAddRequest>(json, LocalAiJson) is { } add
                        && Bound(add.RequestId, add.DocumentRevision, add.DocumentToken))
                        LocalAiChange(source, revision, token, add.RequestId, "add",
                            () => LocalAiRuntimes.AddRemote(LocalAiRuntimes.Ollama, add.Label, add.Endpoint, "homelab"));
                    break;
                case LocalAiPrefix + "remove.v1":
                    if (JsonSerializer.Deserialize<LocalAiRemoveRequest>(json, LocalAiJson) is { } remove
                        && Bound(remove.RequestId, remove.DocumentRevision, remove.DocumentToken))
                        LocalAiChange(source, revision, token, remove.RequestId, "remove",
                            () => LocalAiRuntimes.Remove(remove.Runtime, "homelab"));
                    break;
                case LocalAiPrefix + "answer.v1":
                    if (JsonSerializer.Deserialize<LocalAiAnswerRequest>(json, LocalAiJson) is { } answer
                        && Bound(answer.RequestId, answer.DocumentRevision, answer.DocumentToken))
                        LocalAiChange(source, revision, token, answer.RequestId, "answer",
                            () => LocalAiRuntimes.SetAnswer(answer.Runtime, answer.Model, "homelab"));
                    break;
                case LocalAiPrefix + "chat.v1":
                    if (JsonSerializer.Deserialize<LocalAiChatRequest>(json, LocalAiJson) is { } chat
                        && Bound(chat.RequestId, chat.DocumentRevision, chat.DocumentToken)
                        && chat.Messages is { Length: > 0 and <= LocalAiMaxChatMessages }
                        && !_localAiChats.ContainsKey(chat.RequestId))
                        _ = LocalAiChatAsync(source, revision, token, chat);
                    break;
                case LocalAiPrefix + "chat-cancel.v1":
                    if (JsonSerializer.Deserialize<LocalAiRequest>(json, LocalAiJson) is { } cancel
                        && Bound(cancel.RequestId, cancel.DocumentRevision, cancel.DocumentToken)
                        && _localAiChats.TryGetValue(cancel.RequestId, out var running))
                        running.Cancel();
                    break;
            }
        }
        catch (Exception error) when (error is JsonException or ArgumentException or InvalidOperationException or NotSupportedException) { }
    }

    private void LocalAiChange(string source, long revision, string token, string requestId, string operation, Func<LocalAiRuntimesSnapshot> change)
    {
        LocalAiRuntimesSnapshot snapshot;
        try { snapshot = change(); }
        catch (ArgumentException error) when (error.ParamName == "endpoint") { LocalAiRefuse(source, revision, token, requestId, operation, "invalid_endpoint"); return; }
        catch (ArgumentException) { LocalAiRefuse(source, revision, token, requestId, operation, "invalid"); return; }
        catch (InvalidOperationException) { LocalAiRefuse(source, revision, token, requestId, operation, "unreadable"); return; }
        catch (Exception error) when (error is IOException or UnauthorizedAccessException) { LocalAiRefuse(source, revision, token, requestId, operation, "failed"); return; }
        _ = LocalAiReplyAsync(source, revision, token, requestId, operation, snapshot);
    }

    private static async Task<Dictionary<string, LocalAiRuntimeStatus>> LocalAiProbeAsync(LocalAiRuntimesSnapshot snapshot)
    {
        var enabled = snapshot.Runtimes.Where(r => r.Enabled).ToList();
        var statuses = await Task.WhenAll(enabled.Select(r => LocalAiRuntimes.ProbeAsync(r)));
        return statuses.ToDictionary(s => s.Id, StringComparer.Ordinal);
    }

    private async Task LocalAiReplyAsync(string source, long revision, string token, string requestId, string operation, LocalAiRuntimesSnapshot snapshot)
    {
        var statuses = await LocalAiProbeAsync(snapshot);
        using var parsed = JsonDocument.Parse(LocalAiRuntimes.ToJson(snapshot, statuses));
        var body = parsed.RootElement.Clone();
        BeginInvoke(new Action(() =>
        {
            if (!IsCurrentDeviceContextDocument(source, revision, token)) return;
            PostLocalAi(new Dictionary<string, object?>
            {
                ["type"] = LocalAiResult, ["request_id"] = requestId, ["document_revision"] = revision,
                ["operation"] = operation, ["status"] = "ok", ["local_ai"] = body,
            });
        }));
    }

    // One turn on the runtime the person chose (rule 9). When it cannot answer, the page gets the
    // reason and nothing is sent anywhere else.
    private async Task LocalAiChatAsync(string source, long revision, string token, LocalAiChatRequest chat)
    {
        var cancel = new CancellationTokenSource();
        _localAiChats[chat.RequestId] = cancel;
        try
        {
            var snapshot = LocalAiRuntimes.Read();
            var runtime = snapshot.Answer is { } choice ? snapshot.TryFind(choice.Runtime) : null;
            var status = runtime is { Enabled: true } ? await LocalAiRuntimes.ProbeAsync(runtime, cancel.Token) : null;
            var state = LocalAiRuntimes.AnswerState(snapshot, status);
            if (state is null || !state.Ready)
            {
                LocalAiChatDone(source, revision, token, chat.RequestId, state, state?.Model ?? "", state?.Reason ?? "no_answer");
                return;
            }
            var messages = chat.Messages
                .Where(m => m.Role is "system" or "user" or "assistant" && m.Content is not null)
                .Select(m => new LocalAiChatMessage(m.Role, m.Content))
                .ToList();
            var result = await LocalAiRuntimes.ChatAsync(state.Runtime, state.Model, messages,
                piece => BeginInvoke(new Action(() =>
                {
                    if (!IsCurrentDeviceContextDocument(source, revision, token)) return;
                    PostLocalAi(new Dictionary<string, object?>
                    {
                        ["type"] = LocalAiChatDelta, ["request_id"] = chat.RequestId, ["document_revision"] = revision, ["text"] = piece,
                    });
                })), cancel.Token);
            LocalAiChatDone(source, revision, token, chat.RequestId, state, result.Model, result.Answered ? "" : result.Error);
        }
        finally
        {
            BeginInvoke(new Action(() =>
            {
                _localAiChats.Remove(chat.RequestId);
                cancel.Dispose();
            }));
        }
    }

    private void LocalAiChatDone(string source, long revision, string token, string requestId, LocalAiAnswerState? state, string model, string error)
    {
        BeginInvoke(new Action(() =>
        {
            if (!IsCurrentDeviceContextDocument(source, revision, token)) return;
            var message = new Dictionary<string, object?>
            {
                ["type"] = LocalAiResult, ["request_id"] = requestId, ["document_revision"] = revision,
                ["operation"] = "chat", ["status"] = error.Length == 0 ? "ok" : "refused",
                ["runtime"] = state?.Runtime.Id ?? "", ["kind"] = state?.Runtime.Kind ?? "",
                ["label"] = state?.Runtime.Label ?? "", ["remote"] = state?.Runtime.Remote ?? false, ["model"] = model,
            };
            if (error.Length > 0) message["error"] = error;
            PostLocalAi(message);
        }));
    }

    private void LocalAiRefuse(string source, long revision, string token, string requestId, string operation, string error)
    {
        if (!IsCurrentDeviceContextDocument(source, revision, token)) return;
        PostLocalAi(new Dictionary<string, object?>
        {
            ["type"] = LocalAiResult, ["request_id"] = requestId, ["document_revision"] = revision,
            ["operation"] = operation, ["status"] = "refused", ["error"] = error,
        });
    }

    private void PostLocalAi(Dictionary<string, object?> message)
    {
        try { _webView.CoreWebView2.PostWebMessageAsJson(JsonSerializer.Serialize(message)); }
        catch (Exception error) when (error is InvalidOperationException or ArgumentException
            or System.Runtime.InteropServices.COMException or ObjectDisposedException) { /* the document is gone */ }
    }
}
