using System.Text.Json;
using System.Text.Json.Serialization;
using Kombify.Client.Shell;
using Microsoft.Web.WebView2.Core;

namespace Kombify.Homelab.Client;

// Settings › AI providers: the device-wide local AI runtimes (workspace
// platform/standards/local-ai-runtimes.v1.md) through Kombify.Client.Shell LocalAiRuntimes. The page
// talks to this window through two fixed, typed messages, gated like the device-context read (the
// current document of the product's own authority, its revision and token); answers go to that
// document only. The shell probes a switched-on runtime itself, so the page needs no access to it.
//
//   read     the file, with the status of each switched-on runtime
//   write    one runtime's switch or server address; the file after the change, or a refusal
//
// A change of the file by any app sends `changed` (no data) to the current document, which reads again.
internal sealed partial class ClientWindow
{
    private const string LocalAiPrefix = "kombify.homelab.local-ai.";
    private const string LocalAiResult = LocalAiPrefix + "result.v1";
    private const string LocalAiChanged = LocalAiPrefix + "changed.v1";
    private const int LocalAiMaxMessageChars = 4096;

    private sealed record LocalAiRequest(string Type, string RequestId, long DocumentRevision, string DocumentToken);
    private sealed record LocalAiWriteRequest(string Type, string RequestId, long DocumentRevision, string DocumentToken,
        string Runtime, bool? Enabled = null, string? Endpoint = null);

    private static readonly JsonSerializerOptions LocalAiJson = new()
    {
        PropertyNamingPolicy = JsonNamingPolicy.SnakeCaseLower,
        UnmappedMemberHandling = JsonUnmappedMemberHandling.Disallow,
        RespectRequiredConstructorParameters = true,
    };

    private IDisposable? _localAiWatch;

    private void RegisterLocalAiBridge()
    {
        _webView.CoreWebView2.WebMessageReceived += OnLocalAiRequested;
        try { _localAiWatch = LocalAiRuntimes.Watch(() => BeginInvoke(new Action(NotifyLocalAiChanged))); }
        catch (Exception error) when (error is IOException or UnauthorizedAccessException or ArgumentException) { /* the page still reads on open */ }
        FormClosed += (_, _) => _localAiWatch?.Dispose();
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
            if (json.Length > LocalAiMaxMessageChars) return;
            using var peek = JsonDocument.Parse(json);
            if (peek.RootElement.ValueKind != JsonValueKind.Object
                || !peek.RootElement.TryGetProperty("type", out var typeElement)
                || typeElement.ValueKind != JsonValueKind.String) return;
            switch (typeElement.GetString())
            {
                case LocalAiPrefix + "read.v1":
                    if (JsonSerializer.Deserialize<LocalAiRequest>(json, LocalAiJson) is { } read
                        && Guid.TryParse(read.RequestId, out _) && read.DocumentRevision == revision && read.DocumentToken == token)
                        _ = LocalAiReplyAsync(source, revision, token, read.RequestId, "read", LocalAiRuntimes.Read());
                    break;
                case LocalAiPrefix + "write.v1":
                    if (JsonSerializer.Deserialize<LocalAiWriteRequest>(json, LocalAiJson) is { } write
                        && Guid.TryParse(write.RequestId, out _) && write.DocumentRevision == revision && write.DocumentToken == token)
                        LocalAiWrite(source, revision, token, write);
                    break;
            }
        }
        catch (Exception error) when (error is JsonException or ArgumentException or InvalidOperationException) { }
    }

    private void LocalAiWrite(string source, long revision, string token, LocalAiWriteRequest write)
    {
        LocalAiRuntimesSnapshot snapshot;
        try { snapshot = LocalAiRuntimes.Set(write.Runtime, write.Enabled, write.Endpoint, "homelab"); }
        catch (ArgumentException) { LocalAiRefuse(source, revision, token, write.RequestId, "invalid_endpoint"); return; }
        catch (InvalidOperationException) { LocalAiRefuse(source, revision, token, write.RequestId, "unreadable"); return; }
        catch (Exception error) when (error is IOException or UnauthorizedAccessException) { LocalAiRefuse(source, revision, token, write.RequestId, "failed"); return; }
        _ = LocalAiReplyAsync(source, revision, token, write.RequestId, "write", snapshot);
    }

    private async Task LocalAiReplyAsync(string source, long revision, string token, string requestId, string operation, LocalAiRuntimesSnapshot snapshot)
    {
        var statuses = new Dictionary<string, LocalAiRuntimeStatus>();
        foreach (var runtime in snapshot.Runtimes.Where(r => r.Enabled))
            statuses[runtime.Id] = await LocalAiRuntimes.ProbeAsync(runtime);
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

    private void LocalAiRefuse(string source, long revision, string token, string requestId, string error)
    {
        if (!IsCurrentDeviceContextDocument(source, revision, token)) return;
        PostLocalAi(new Dictionary<string, object?>
        {
            ["type"] = LocalAiResult, ["request_id"] = requestId, ["document_revision"] = revision,
            ["operation"] = "write", ["status"] = "refused", ["error"] = error,
        });
    }

    private void PostLocalAi(Dictionary<string, object?> message)
    {
        try { _webView.CoreWebView2.PostWebMessageAsJson(JsonSerializer.Serialize(message)); }
        catch (Exception error) when (error is InvalidOperationException or ArgumentException
            or System.Runtime.InteropServices.COMException or ObjectDisposedException) { /* the document is gone */ }
    }
}
