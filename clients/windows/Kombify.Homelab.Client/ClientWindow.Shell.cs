using System.Diagnostics;
using System.Text.Json;
using System.Text.Json.Serialization;
using Microsoft.Web.WebView2.Core;

namespace Kombify.Homelab.Client;

// What the shell itself offers a person (concept "when something does not work", DESKTOP-FOUNDATION-STANDARD
// §5.7). Two kinds of documents talk to it:
//
//   the shell's own failure page (about:blank, drawn by RenderFailure)
//     retry          "Try again": runs again what failed (start, server connection, sign-in, a blocked recovery)
//     help           "Get help": the public support page in the system browser, no account needed
//
//   the product's own document (gated like the device-context read: its revision and token)
//     update         the update state now, and again under the same request id at every change:
//                    "ready" (a verified update is staged: the page shows "Restart to update"),
//                    "updated" (this start applied one: the page shows a short note, once) or "none"
//     restart        closes the client and starts it again, so the staged update applies before anything runs
//     open-account   "With my kombify account" on the first start: kombify's own Homelab sign-in in the browser
//
// No URL, path or version of another program comes from a page; the shell answers with fixed values.
internal sealed partial class ClientWindow
{
    private const string ShellPrefix = "kombify.homelab.shell.";
    private const string ShellResult = ShellPrefix + "result.v1";
    private const string SupportUrl = "https://kombify.io/support";
    private const string AccountSignInUrl = "https://homelab.kombify.io/login?client=windows";
    private static readonly TimeSpan UpdateCheckInterval = TimeSpan.FromHours(6);

    private sealed record ShellRequest(string Type, string RequestId, long DocumentRevision, string DocumentToken);

    private static readonly JsonSerializerOptions ShellJson = new()
    {
        PropertyNamingPolicy = JsonNamingPolicy.SnakeCaseLower,
        UnmappedMemberHandling = JsonUnmappedMemberHandling.Disallow,
        RespectRequiredConstructorParameters = true,
        RespectNullableAnnotations = true
    };

    // What "Try again" on the failure page now shown runs; null while no failure page is shown.
    private Action? _failureRetry;
    private string? _updateReady;
    private bool _updateNoteDelivered;
    private bool _restarting;
    private CompanionDocument? _updateDocument;

    private void RegisterShellBridge()
    {
        _webView.CoreWebView2.NavigationStarting += (_, args) =>
        {
            if (args.Cancel) return;
            _updateDocument = null;
            if (args.Uri != "about:blank") _failureRetry = null;
        };
        _webView.CoreWebView2.WebMessageReceived += OnShellRequested;
    }

    private void ShowFailure(string title, string technicalDetail, string technicalHtml, Action retry)
    {
        if (InvokeRequired)
        {
            BeginInvoke(new Action(() => ShowFailure(title, technicalDetail, technicalHtml, retry)));
            return;
        }
        if (IsDisposed) return;
        NavigateHtml(RenderFailure(title, technicalDetail, technicalHtml));
        _failureRetry = retry;
    }

    private void RetryOpenProduct() => _ = OpenProductAsync();

    // Ends this client and starts it again: a staged update or a pending recovery runs before the runtime starts.
    private void RestartClient()
    {
        if (_restarting) return;
        _restarting = true;
        NavigateHtml(RenderProgress(ShellText.Get("RestartingTitle")));
        _ = Task.Run(StopStartedLocalRuntime).ContinueWith(_ => BeginInvoke(new Action(() =>
        {
            try { Program.StartAgainAfterExit(); }
            catch (Exception error) when (error is InvalidOperationException or System.ComponentModel.Win32Exception)
            {
                _restarting = false;
                ShowFailure(ShellText.Get("StartFailedTitle"), error.Message, "", RestartClient);
                return;
            }
            Close();
        })), TaskScheduler.Default);
    }

    private void OnShellRequested(object? sender, CoreWebView2WebMessageReceivedEventArgs args)
    {
        string type;
        try
        {
            var json = args.WebMessageAsJson;
            if (json.Length > 1024) return;
            using var peek = JsonDocument.Parse(json);
            if (peek.RootElement.ValueKind != JsonValueKind.Object
                || !peek.RootElement.TryGetProperty("type", out var typeElement)
                || typeElement.ValueKind != JsonValueKind.String
                || typeElement.GetString() is not { } named || !named.StartsWith(ShellPrefix, StringComparison.Ordinal)) return;
            type = named;

            // The failure page is the shell's own document; it is the one about:blank this window shows.
            if (type is ShellPrefix + "retry.v1" or ShellPrefix + "help.v1")
            {
                if (args.Source != "about:blank" || _webView.CoreWebView2.Source != "about:blank" || _failureRetry is null) return;
                if (type == ShellPrefix + "help.v1") OpenExternal(SupportUrl);
                else
                {
                    var retry = _failureRetry;
                    _failureRetry = null;
                    retry();
                }
                return;
            }

            var source = args.Source;
            var revision = _deviceContextDocumentRevision;
            var token = _deviceContextDocumentToken;
            if (!IsCurrentDeviceContextDocument(source, revision, token)) return;
            var request = JsonSerializer.Deserialize<ShellRequest>(json, ShellJson);
            if (request is null) return;
            var document = Bound(request.RequestId, request.DocumentRevision, request.DocumentToken, source, revision, token);
            if (document is null) return;
            switch (type)
            {
                case ShellPrefix + "update.v1":
                    _updateDocument = document;
                    ReplyUpdateState();
                    break;
                case ShellPrefix + "restart.v1":
                    if (_updateReady is not null) RestartClient();
                    break;
                case ShellPrefix + "open-account.v1":
                    OpenExternal(AccountSignInUrl);
                    break;
            }
        }
        catch (Exception error) when (error is JsonException or ArgumentException or InvalidOperationException) { }
    }

    // Checks the signed update feed at start and every few hours while the window is open. A verified
    // staged update is offered as "Restart to update"; it also applies by itself at the next start.
    private async Task CheckForUpdateAsync()
    {
        while (!IsDisposed)
        {
            await ClientUpdater.CheckAndStageAsync(_config, ResolveRuntimeExecutablePath());
            var ready = ClientUpdater.ReadyUpdateVersion(_config);
            if (IsDisposed) return;
            if (ready is not null)
            {
                BeginInvoke(new Action(() =>
                {
                    _updateReady = ready;
                    ReplyUpdateState();
                }));
                return;
            }
            await Task.Delay(UpdateCheckInterval);
        }
    }

    private void ReplyUpdateState()
    {
        if (_updateDocument is not { } document || !CompanionStillShown(document)) return;
        var state = "none";
        var version = "";
        if (_updateReady is not null)
        {
            state = "ready";
            version = _updateReady;
        }
        else if (!_updateNoteDelivered && ClientUpdater.AppliedVersion is { } applied)
        {
            state = "updated";
            version = applied;
            _updateNoteDelivered = true;
        }
        var message = new Dictionary<string, object?>
        {
            ["type"] = ShellResult,
            ["request_id"] = document.RequestId,
            ["document_revision"] = document.Revision,
            ["operation"] = "update",
            ["status"] = "ok",
            ["state"] = state,
            ["version"] = version,
        };
        try { _webView.CoreWebView2.PostWebMessageAsJson(JsonSerializer.Serialize(message)); }
        catch (Exception error) when (error is InvalidOperationException or ArgumentException
            or System.Runtime.InteropServices.COMException or ObjectDisposedException) { /* the document is gone */ }
    }

    private static void OpenExternal(string url)
    {
        try
        {
            using var _ = Process.Start(new ProcessStartInfo { FileName = url, UseShellExecute = true });
        }
        catch (Exception error) when (error is InvalidOperationException or System.ComponentModel.Win32Exception) { }
    }
}
