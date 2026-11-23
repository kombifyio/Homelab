using System.Net.Http.Json;
using System.Text;
using System.Text.Json;
using System.Text.Json.Nodes;
using System.Text.RegularExpressions;

namespace Kombify.Client.Shell;

// The device-wide local AI runtimes of one Windows user (workspace
// platform/standards/local-ai-runtimes.v1.md): %LOCALAPPDATA%\kombify\local-ai.json,
// shared with Workbench Desktop's server and Kubi. Reads never fail (an unreadable file is
// reported, every runtime off); writes keep every member they do not know and are
// refused for an unreadable file. Nothing here installs or starts a runtime.
//
// Besides the rule-5 runtimes of this device (`ollama`), the file may hold runtimes on another
// machine of the person (rule 2: `ollama-<name>` with `kind` and `label`, such as the Ollama app
// on their homelab server), and `answer` (rule 9): the runtime and model the apps that answer
// from one source answer with. `ChatAsync` runs a conversation on that runtime.

/// <summary>
/// One runtime as the file holds it; an absent rule-5 entry reads as off at the default endpoint.
/// <see cref="Kind"/> is the rule-5 runtime it speaks; <see cref="Label"/> the name a person sees
/// (empty for the device's own runtime); <see cref="Remote"/> marks a runtime on another machine.
/// </summary>
public sealed record LocalAiRuntime(string Id, bool Enabled, string Endpoint)
{
    public string Kind { get; init; } = Id;
    public string Label { get; init; } = "";
    public bool Remote => !LocalAiRuntimes.Known.Contains(Id);
}

/// <summary>The answer choice of rule 9: the runtime row and model; <see cref="Model"/> empty means the first one the runtime reports.</summary>
public sealed record LocalAiAnswer(string Runtime, string Model);

/// <summary>The file at one moment. <see cref="Readable"/> is false for a version other than 1 or broken JSON.</summary>
public sealed record LocalAiRuntimesSnapshot(bool Readable, long Revision, string ChangedBy, IReadOnlyList<LocalAiRuntime> Runtimes)
{
    /// <summary>The answer choice, or null when the apps answer from the account's AI use.</summary>
    public LocalAiAnswer? Answer { get; init; }

    public LocalAiRuntime Find(string id) => Runtimes.First(r => r.Id == id);

    public LocalAiRuntime? TryFind(string id) => Runtimes.FirstOrDefault(r => r.Id == id);
}

/// <summary>
/// What a runtime reported: reachable with its version and models, or the reason it was not.
/// <see cref="ChatModels"/> are the models that can hold a conversation (no embedding models), in the
/// runtime's order; the first of them answers when the answer choice names no model.
/// </summary>
public sealed record LocalAiRuntimeStatus(string Id, bool Reachable, string Version, IReadOnlyList<string> Models, string Error)
{
    public IReadOnlyList<string> ChatModels { get; init; } = Models;
}

/// <summary>
/// Whether the answer choice can answer now. <see cref="Reason"/> is empty when it can, else one of
/// <c>runtime_missing</c>, <c>runtime_off</c>, <c>unreachable</c>, <c>no_models</c>, <c>model_missing</c>.
/// </summary>
public sealed record LocalAiAnswerState(LocalAiRuntime Runtime, string Model, string Reason)
{
    public bool Ready => Reason.Length == 0;
}

/// <summary>One message of a conversation: <c>system</c>, <c>user</c> or <c>assistant</c>.</summary>
public sealed record LocalAiChatMessage(string Role, string Content);

/// <summary>The end of a conversation turn: the model that answered, or the reason it did not.</summary>
public sealed record LocalAiChatResult(bool Answered, string Model, string Error);

public static partial class LocalAiRuntimes
{
    public const string Ollama = "ollama";
    public const int Version = 1;
    public const int MaxLabelLength = 80;

    private static readonly IReadOnlyDictionary<string, string> DefaultEndpoints =
        new Dictionary<string, string> { [Ollama] = "http://127.0.0.1:11434" };

    private static readonly string[] Writers = ["workbench", "paperwork", "homelab", "kubi"];

    /// <summary>The runtimes contract v1 declares, in display order.</summary>
    public static IReadOnlyList<string> Known { get; } = [Ollama];

    [GeneratedRegex("^[a-z][a-z0-9-]{0,31}$")]
    private static partial Regex RuntimeId();

    [GeneratedRegex("^[A-Za-z0-9._:@/+-]{1,160}$")]
    private static partial Regex ModelId();

    public static string Directory => Path.Combine(
        Environment.GetFolderPath(Environment.SpecialFolder.LocalApplicationData), "kombify");

    public static string StorePath => Path.Combine(Directory, "local-ai.json");

    public static string DefaultEndpoint(string id) =>
        DefaultEndpoints.TryGetValue(id, out var endpoint) ? endpoint : throw new ArgumentException($"Unknown local AI runtime '{id}'.");

    /// <summary>The default endpoint of a rule-5 runtime; empty for a runtime on another machine.</summary>
    public static string DefaultEndpointOrEmpty(string id) => DefaultEndpoints.TryGetValue(id, out var endpoint) ? endpoint : "";

    /// <summary>A model id of the contract's shape (<c>qwen2.5:3b</c>, <c>library/llama3.1:8b</c>).</summary>
    public static bool ValidModel(string? value) => value is not null && ModelId().IsMatch(value);

    /// <summary>An absolute http(s) URL without query or fragment, without a trailing slash; null when invalid.</summary>
    public static string? NormalizeEndpoint(string? value)
    {
        if (string.IsNullOrWhiteSpace(value) || value.Length > 2048) return null;
        if (!Uri.TryCreate(value.Trim(), UriKind.Absolute, out var uri)) return null;
        if (uri.Scheme is not ("http" or "https") || uri.Query.Length > 0 || uri.Fragment.Length > 0 || uri.UserInfo.Length > 0) return null;
        return uri.GetLeftPart(UriPartial.Path).TrimEnd('/');
    }

    public static LocalAiRuntimesSnapshot Read()
    {
        var root = ReadRoot(out var readable);
        if (!readable) return new(false, 0, "", Known.Select(id => new LocalAiRuntime(id, false, DefaultEndpoint(id))).ToList());
        return Snapshot(root);
    }

    /// <summary>
    /// Sets the given members of one runtime and returns the file after the write.
    /// Throws <see cref="InvalidOperationException"/> for an unreadable file and
    /// <see cref="ArgumentException"/> for an invalid endpoint or an unknown runtime.
    /// </summary>
    public static LocalAiRuntimesSnapshot Set(string id, bool? enabled, string? endpoint, string changedBy)
    {
        string? normalized = null;
        if (endpoint is not null && (normalized = NormalizeEndpoint(endpoint)) is null)
            throw new ArgumentException("The server address must be an http or https URL.", nameof(endpoint));
        return Write(changedBy, root =>
        {
            var runtimes = Runtimes(root);
            if (!Known.Contains(id) && runtimes[id] is not JsonObject) throw new ArgumentException($"Unknown local AI runtime '{id}'.");
            var entry = runtimes[id] as JsonObject ?? [];
            runtimes[id] = entry;
            if (enabled is { } on) entry["enabled"] = on;
            if (normalized is not null) entry["endpoint"] = normalized;
            else if (entry["endpoint"] is null) entry["endpoint"] = DefaultEndpoint(id);
        });
    }

    /// <summary>
    /// Adds a runtime on another machine of the person (rule 2), switched on, and returns the file
    /// after the write. An existing row of the same kind and endpoint is switched on and renamed
    /// instead of added twice. The id is <c>&lt;kind&gt;-&lt;name&gt;</c> from the label.
    /// </summary>
    public static LocalAiRuntimesSnapshot AddRemote(string kind, string label, string endpoint, string changedBy)
    {
        if (!Known.Contains(kind)) throw new ArgumentException($"Unknown local AI runtime kind '{kind}'.", nameof(kind));
        label = label.Trim();
        if (label.Length is 0 or > MaxLabelLength) throw new ArgumentException("The name must have 1 to 80 characters.", nameof(label));
        var normalized = NormalizeEndpoint(endpoint) ?? throw new ArgumentException("The server address must be an http or https URL.", nameof(endpoint));
        return Write(changedBy, root =>
        {
            var runtimes = Runtimes(root);
            var existing = runtimes
                .Where(pair => pair.Value is JsonObject row && !Known.Contains(pair.Key)
                    && KindOf(pair.Key, row) == kind && Text(row["endpoint"]) == normalized)
                .Select(pair => pair.Key)
                .FirstOrDefault();
            var id = existing ?? UniqueId(runtimes, kind, label);
            var entry = runtimes[id] as JsonObject ?? [];
            runtimes[id] = entry;
            entry["kind"] = kind;
            entry["label"] = label;
            entry["enabled"] = true;
            entry["endpoint"] = normalized;
        });
    }

    /// <summary>Removes a runtime on another machine; an <c>answer</c> that names it goes with it (rule 9).</summary>
    public static LocalAiRuntimesSnapshot Remove(string id, string changedBy)
    {
        if (Known.Contains(id)) throw new ArgumentException("The device's own runtime stays; switch it off instead.", nameof(id));
        return Write(changedBy, root =>
        {
            var runtimes = Runtimes(root);
            if (runtimes[id] is not JsonObject) throw new ArgumentException($"Unknown local AI runtime '{id}'.", nameof(id));
            runtimes.Remove(id);
            if (root["answer"] is JsonObject answer && Text(answer["runtime"]) == id) root.Remove("answer");
        });
    }

    /// <summary>
    /// Sets the answer choice of rule 9: <paramref name="runtime"/> null lets the apps answer from the
    /// account's AI use again; <paramref name="model"/> null or empty is the runtime's first model.
    /// </summary>
    public static LocalAiRuntimesSnapshot SetAnswer(string? runtime, string? model, string changedBy)
    {
        if (!string.IsNullOrEmpty(model) && !ValidModel(model)) throw new ArgumentException("Unknown model.", nameof(model));
        return Write(changedBy, root =>
        {
            if (runtime is null)
            {
                root.Remove("answer");
                return;
            }
            if (!Known.Contains(runtime) && Runtimes(root)[runtime] is not JsonObject)
                throw new ArgumentException($"Unknown local AI runtime '{runtime}'.", nameof(runtime));
            var answer = new JsonObject { ["runtime"] = runtime };
            if (!string.IsNullOrEmpty(model)) answer["model"] = model;
            root["answer"] = answer;
        });
    }

    /// <summary>
    /// Whether the answer choice can answer now, from the file and the runtime's status:
    /// null when the file holds no answer choice.
    /// </summary>
    public static LocalAiAnswerState? AnswerState(LocalAiRuntimesSnapshot snapshot, LocalAiRuntimeStatus? status)
    {
        if (snapshot.Answer is not { } answer) return null;
        var runtime = snapshot.TryFind(answer.Runtime);
        if (runtime is null) return new(new LocalAiRuntime(answer.Runtime, false, "") { Kind = Ollama }, answer.Model, "runtime_missing");
        if (!runtime.Enabled) return new(runtime, answer.Model, "runtime_off");
        if (status is null || !status.Reachable) return new(runtime, answer.Model, "unreachable");
        if (status.ChatModels.Count == 0) return new(runtime, answer.Model, "no_models");
        if (answer.Model.Length == 0) return new(runtime, status.ChatModels[0], "");
        return status.ChatModels.Contains(answer.Model, StringComparer.Ordinal)
            ? new(runtime, answer.Model, "")
            : new(runtime, answer.Model, "model_missing");
    }

    /// <summary>Calls <paramref name="onChange"/> after the file changed (debounced); dispose to stop.</summary>
    public static IDisposable Watch(Action onChange)
    {
        System.IO.Directory.CreateDirectory(Directory);
        var watcher = new FileSystemWatcher(Directory, "local-ai.json")
        {
            NotifyFilter = NotifyFilters.LastWrite | NotifyFilters.FileName | NotifyFilters.Size | NotifyFilters.CreationTime,
        };
        var timer = new System.Threading.Timer(_ => onChange(), null, Timeout.Infinite, Timeout.Infinite);
        void Changed(object? _, FileSystemEventArgs __) => timer.Change(150, Timeout.Infinite);
        watcher.Changed += Changed;
        watcher.Created += Changed;
        watcher.Deleted += Changed;
        watcher.Renamed += (s, e) => Changed(s, e);
        watcher.EnableRaisingEvents = true;
        return new Subscription(watcher, timer);
    }

    /// <summary>Asks the runtime for its version and models (Ollama: <c>/api/version</c>, <c>/api/tags</c>; 3 s).</summary>
    public static async Task<LocalAiRuntimeStatus> ProbeAsync(LocalAiRuntime runtime, CancellationToken cancellationToken = default)
    {
        using var http = new HttpClient { Timeout = TimeSpan.FromSeconds(3) };
        try
        {
            var version = await http.GetFromJsonAsync<JsonObject>($"{runtime.Endpoint}/api/version", cancellationToken).ConfigureAwait(false);
            var tags = await http.GetFromJsonAsync<JsonObject>($"{runtime.Endpoint}/api/tags", cancellationToken).ConfigureAwait(false);
            var entries = (tags?["models"] as JsonArray ?? []).OfType<JsonObject>().ToList();
            var models = entries.Select(model => Text(model["name"])).OfType<string>().ToList();
            var chat = entries.Where(CanChat).Select(model => Text(model["name"])).OfType<string>().ToList();
            return new(runtime.Id, true, version?["version"]?.GetValue<string>() ?? "", models, "") { ChatModels = chat };
        }
        catch (Exception error) when (error is HttpRequestException or TaskCanceledException or JsonException or InvalidOperationException or NotSupportedException or UriFormatException)
        {
            return new(runtime.Id, false, "", [], error is TaskCanceledException ? "timeout" : "unreachable");
        }
    }

    /// <summary>
    /// Runs one conversation turn on the runtime (Ollama <c>POST /api/chat</c>, streamed) and hands
    /// every piece of the answer to <paramref name="onDelta"/> as it arrives. The answer is the
    /// runtime's alone; nothing goes anywhere else.
    /// </summary>
    public static async Task<LocalAiChatResult> ChatAsync(LocalAiRuntime runtime, string model, IReadOnlyList<LocalAiChatMessage> messages,
        Action<string> onDelta, CancellationToken cancellationToken = default)
    {
        if (!ValidModel(model)) return new(false, model, "model_missing");
        var body = new JsonObject
        {
            ["model"] = model,
            ["stream"] = true,
            ["messages"] = new JsonArray(messages
                .Where(m => m.Role is "system" or "user" or "assistant")
                .Select(m => (JsonNode?)new JsonObject { ["role"] = m.Role, ["content"] = m.Content })
                .ToArray()),
        };
        // No overall timeout: a long answer streams for as long as it needs; the caller cancels.
        using var http = new HttpClient { Timeout = Timeout.InfiniteTimeSpan };
        using var connect = CancellationTokenSource.CreateLinkedTokenSource(cancellationToken);
        connect.CancelAfter(TimeSpan.FromSeconds(120));
        try
        {
            using var request = new HttpRequestMessage(HttpMethod.Post, $"{runtime.Endpoint}/api/chat")
            {
                Content = new StringContent(body.ToJsonString(), Encoding.UTF8, "application/json"),
            };
            using var response = await http.SendAsync(request, HttpCompletionOption.ResponseHeadersRead, connect.Token).ConfigureAwait(false);
            if (!response.IsSuccessStatusCode)
                return new(false, model, response.StatusCode == System.Net.HttpStatusCode.NotFound ? "model_missing" : "failed");
            await using var stream = await response.Content.ReadAsStreamAsync(cancellationToken).ConfigureAwait(false);
            using var reader = new StreamReader(stream, Encoding.UTF8);
            var answeredBy = model;
            while (await reader.ReadLineAsync(cancellationToken).ConfigureAwait(false) is { } line)
            {
                if (line.Length == 0) continue;
                if (JsonNode.Parse(line) is not JsonObject chunk) continue;
                if (Text(chunk["error"]) is { Length: > 0 }) return new(false, model, "failed");
                if (Text(chunk["model"]) is { Length: > 0 } name) answeredBy = name;
                if (Text(chunk["message"]?["content"]) is { Length: > 0 } piece) onDelta(piece);
                if (chunk["done"] is JsonValue done && done.TryGetValue<bool>(out var finished) && finished) break;
            }
            return new(true, answeredBy, "");
        }
        catch (OperationCanceledException) when (cancellationToken.IsCancellationRequested)
        {
            return new(false, model, "cancelled");
        }
        catch (Exception error) when (error is HttpRequestException or OperationCanceledException or IOException or JsonException or UriFormatException)
        {
            return new(false, model, "unreachable");
        }
    }

    // An embedding model (bge-m3, nomic-embed-text) is listed like any other but cannot answer. Ollama
    // names a model's capabilities (`completion`, `embedding`); an older runtime without them is judged
    // by its family (BERT) and name.
    private static bool CanChat(JsonObject model)
    {
        if (model["capabilities"] is JsonArray capabilities)
            return capabilities.Any(c => Text(c) == "completion");
        var family = Text(model["details"]?["family"]) ?? "";
        var name = Text(model["name"]) ?? "";
        return !family.Contains("bert", StringComparison.OrdinalIgnoreCase)
            && !name.Contains("embed", StringComparison.OrdinalIgnoreCase);
    }

    /// <summary>The snapshot and optional statuses in the bridge shape of the Windows apps' settings pages.</summary>
    public static string ToJson(LocalAiRuntimesSnapshot snapshot, IReadOnlyDictionary<string, LocalAiRuntimeStatus>? statuses = null)
    {
        var runtimes = new JsonArray();
        foreach (var runtime in snapshot.Runtimes)
        {
            var item = new JsonObject
            {
                ["id"] = runtime.Id,
                ["kind"] = runtime.Kind,
                ["label"] = runtime.Label,
                ["remote"] = runtime.Remote,
                ["enabled"] = runtime.Enabled,
                ["endpoint"] = runtime.Endpoint,
                ["defaultEndpoint"] = DefaultEndpointOrEmpty(runtime.Id),
            };
            if (statuses is not null && statuses.TryGetValue(runtime.Id, out var status))
            {
                item["status"] = new JsonObject
                {
                    ["reachable"] = status.Reachable,
                    ["version"] = status.Version,
                    ["models"] = new JsonArray(status.Models.Select(m => (JsonNode?)JsonValue.Create(m)).ToArray()),
                    ["chatModels"] = new JsonArray(status.ChatModels.Select(m => (JsonNode?)JsonValue.Create(m)).ToArray()),
                    ["error"] = status.Error,
                };
            }
            runtimes.Add(item);
        }
        var json = new JsonObject
        {
            ["readable"] = snapshot.Readable,
            ["revision"] = snapshot.Revision,
            ["changedBy"] = snapshot.ChangedBy,
            ["runtimes"] = runtimes,
        };
        if (snapshot.Answer is { } answer)
        {
            var block = new JsonObject { ["runtime"] = answer.Runtime, ["model"] = answer.Model };
            var state = AnswerState(snapshot, statuses is not null && statuses.TryGetValue(answer.Runtime, out var s) ? s : null);
            if (statuses is not null && state is not null)
            {
                block["resolvedModel"] = state.Model;
                block["reason"] = state.Reason;
            }
            json["answer"] = block;
        }
        return json.ToJsonString();
    }

    private static LocalAiRuntimesSnapshot Write(string changedBy, Action<JsonObject> change)
    {
        if (!Writers.Contains(changedBy)) throw new ArgumentException("Unknown writer.", nameof(changedBy));
        var root = ReadRoot(out var readable);
        if (!readable) throw new InvalidOperationException($"{StorePath} has another version or is damaged; it is left as it is.");
        change(root);
        root["version"] = Version;
        root["revision"] = (TryLong(root["revision"]) ?? 0) + 1;
        root["changedBy"] = changedBy;

        System.IO.Directory.CreateDirectory(Directory);
        var temporary = Path.Combine(Directory, $"local-ai.json.{Guid.NewGuid():N}.tmp");
        File.WriteAllText(temporary, root.ToJsonString(new JsonSerializerOptions { WriteIndented = true }) + "\n");
        File.Move(temporary, StorePath, overwrite: true);
        return Snapshot(root);
    }

    private static JsonObject Runtimes(JsonObject root)
    {
        var runtimes = root["runtimes"] as JsonObject ?? [];
        root["runtimes"] = runtimes;
        return runtimes;
    }

    private static string UniqueId(JsonObject runtimes, string kind, string label)
    {
        var slug = new StringBuilder();
        foreach (var c in label.ToLowerInvariant())
        {
            if (c is >= 'a' and <= 'z' or >= '0' and <= '9') slug.Append(c);
            else if (slug.Length > 0 && slug[^1] != '-') slug.Append('-');
        }
        var name = slug.ToString().Trim('-');
        if (name.Length == 0) name = "server";
        var stem = $"{kind}-{name}";
        if (stem.Length > 29) stem = stem[..29].TrimEnd('-');
        var id = stem;
        for (var n = 2; runtimes[id] is not null || Known.Contains(id); n++) id = $"{stem}-{n}";
        return id;
    }

    private static string KindOf(string id, JsonObject row) => Text(row["kind"]) is { Length: > 0 } kind ? kind : id;

    private static string? Text(JsonNode? node) => node is JsonValue value && value.TryGetValue<string>(out var text) ? text : null;

    private static JsonObject ReadRoot(out bool readable)
    {
        readable = true;
        string text;
        try { text = File.ReadAllText(StorePath); }
        catch (Exception error) when (error is FileNotFoundException or DirectoryNotFoundException) { return new JsonObject { ["version"] = Version }; }
        catch (IOException)
        {
            // A writer is replacing the file this instant; the next change event reads it again.
            Thread.Sleep(50);
            try { text = File.ReadAllText(StorePath); }
            catch (IOException) { readable = false; return []; }
        }
        try
        {
            if (JsonNode.Parse(text) is JsonObject root && TryLong(root["version"]) == Version) return root;
        }
        catch (JsonException) { }
        readable = false;
        return [];
    }

    private static LocalAiRuntimesSnapshot Snapshot(JsonObject root)
    {
        var runtimes = root["runtimes"] as JsonObject;
        var list = Known.Select(id =>
        {
            var entry = runtimes?[id] as JsonObject;
            var enabled = entry?["enabled"] is JsonValue value && value.TryGetValue<bool>(out var on) && on;
            var endpoint = NormalizeEndpoint(Text(entry?["endpoint"])) ?? DefaultEndpoint(id);
            return new LocalAiRuntime(id, enabled, endpoint) { Label = Text(entry?["label"]) ?? "" };
        }).ToList();
        // Runtimes on another machine: a known kind and a valid endpoint, in the file's order.
        foreach (var (id, node) in runtimes ?? [])
        {
            if (Known.Contains(id) || node is not JsonObject entry || !RuntimeId().IsMatch(id)) continue;
            var kind = KindOf(id, entry);
            var endpoint = NormalizeEndpoint(Text(entry["endpoint"]));
            if (!Known.Contains(kind) || endpoint is null) continue;
            var enabled = entry["enabled"] is JsonValue value && value.TryGetValue<bool>(out var on) && on;
            var label = Text(entry["label"]) is { Length: > 0 and <= MaxLabelLength } named ? named : id;
            list.Add(new LocalAiRuntime(id, enabled, endpoint) { Kind = kind, Label = label });
        }
        LocalAiAnswer? answer = null;
        if (root["answer"] is JsonObject block && Text(block["runtime"]) is { } runtime && RuntimeId().IsMatch(runtime))
        {
            var model = Text(block["model"]);
            answer = new LocalAiAnswer(runtime, ValidModel(model) ? model! : "");
        }
        var changedBy = Text(root["changedBy"]) ?? "";
        return new(true, TryLong(root["revision"]) ?? 0, changedBy, list) { Answer = answer };
    }

    private static long? TryLong(JsonNode? node) =>
        node is JsonValue value && value.TryGetValue<long>(out var number) ? number
        : node is JsonValue d && d.TryGetValue<double>(out var real) && real == Math.Floor(real) ? (long)real
        : null;

    private sealed class Subscription(FileSystemWatcher watcher, System.Threading.Timer timer) : IDisposable
    {
        public void Dispose()
        {
            watcher.Dispose();
            timer.Dispose();
        }
    }
}
