using System.Net.Http.Json;
using System.Text.Json;
using System.Text.Json.Nodes;

namespace Kombify.Client.Shell;

// The device-wide local AI runtimes of one Windows user (workspace
// platform/standards/local-ai-runtimes.v1.md): %LOCALAPPDATA%\kombify\local-ai.json,
// shared with Workbench Desktop's server. Reads never fail (an unreadable file is
// reported, every runtime off); writes keep every member they do not know and are
// refused for an unreadable file. Nothing here installs or starts a runtime.

/// <summary>One runtime as the file holds it; an absent entry reads as off at the default endpoint.</summary>
public sealed record LocalAiRuntime(string Id, bool Enabled, string Endpoint);

/// <summary>The file at one moment. <see cref="Readable"/> is false for a version other than 1 or broken JSON.</summary>
public sealed record LocalAiRuntimesSnapshot(bool Readable, long Revision, string ChangedBy, IReadOnlyList<LocalAiRuntime> Runtimes)
{
    public LocalAiRuntime Find(string id) => Runtimes.First(r => r.Id == id);
}

/// <summary>What a runtime reported: reachable with its version and models, or the reason it was not.</summary>
public sealed record LocalAiRuntimeStatus(string Id, bool Reachable, string Version, IReadOnlyList<string> Models, string Error);

public static class LocalAiRuntimes
{
    public const string Ollama = "ollama";
    public const int Version = 1;

    private static readonly IReadOnlyDictionary<string, string> DefaultEndpoints =
        new Dictionary<string, string> { [Ollama] = "http://127.0.0.1:11434" };

    /// <summary>The runtimes contract v1 declares, in display order.</summary>
    public static IReadOnlyList<string> Known { get; } = [Ollama];

    public static string Directory => Path.Combine(
        Environment.GetFolderPath(Environment.SpecialFolder.LocalApplicationData), "kombify");

    public static string StorePath => Path.Combine(Directory, "local-ai.json");

    public static string DefaultEndpoint(string id) =>
        DefaultEndpoints.TryGetValue(id, out var endpoint) ? endpoint : throw new ArgumentException($"Unknown local AI runtime '{id}'.");

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
        _ = DefaultEndpoint(id);
        if (changedBy is not ("workbench" or "paperwork" or "homelab")) throw new ArgumentException("Unknown writer.", nameof(changedBy));
        string? normalized = null;
        if (endpoint is not null && (normalized = NormalizeEndpoint(endpoint)) is null)
            throw new ArgumentException("The server address must be an http or https URL.", nameof(endpoint));

        var root = ReadRoot(out var readable);
        if (!readable) throw new InvalidOperationException($"{StorePath} has another version or is damaged; it is left as it is.");
        var runtimes = root["runtimes"] as JsonObject ?? [];
        root["runtimes"] = runtimes;
        var entry = runtimes[id] as JsonObject ?? [];
        runtimes[id] = entry;
        if (enabled is { } on) entry["enabled"] = on;
        if (normalized is not null) entry["endpoint"] = normalized;
        else if (entry["endpoint"] is null) entry["endpoint"] = DefaultEndpoint(id);
        root["version"] = Version;
        root["revision"] = (TryLong(root["revision"]) ?? 0) + 1;
        root["changedBy"] = changedBy;

        System.IO.Directory.CreateDirectory(Directory);
        var temporary = Path.Combine(Directory, $"local-ai.json.{Guid.NewGuid():N}.tmp");
        File.WriteAllText(temporary, root.ToJsonString(new JsonSerializerOptions { WriteIndented = true }) + "\n");
        File.Move(temporary, StorePath, overwrite: true);
        return Snapshot(root);
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
            var models = (tags?["models"] as JsonArray ?? [])
                .Select(model => model?["name"]?.GetValue<string>())
                .OfType<string>()
                .ToList();
            return new(runtime.Id, true, version?["version"]?.GetValue<string>() ?? "", models, "");
        }
        catch (Exception error) when (error is HttpRequestException or TaskCanceledException or JsonException or InvalidOperationException or NotSupportedException)
        {
            return new(runtime.Id, false, "", [], error is TaskCanceledException ? "timeout" : "unreachable");
        }
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
                ["enabled"] = runtime.Enabled,
                ["endpoint"] = runtime.Endpoint,
                ["defaultEndpoint"] = DefaultEndpoint(runtime.Id),
            };
            if (statuses is not null && statuses.TryGetValue(runtime.Id, out var status))
            {
                item["status"] = new JsonObject
                {
                    ["reachable"] = status.Reachable,
                    ["version"] = status.Version,
                    ["models"] = new JsonArray(status.Models.Select(m => (JsonNode?)JsonValue.Create(m)).ToArray()),
                    ["error"] = status.Error,
                };
            }
            runtimes.Add(item);
        }
        return new JsonObject
        {
            ["readable"] = snapshot.Readable,
            ["revision"] = snapshot.Revision,
            ["changedBy"] = snapshot.ChangedBy,
            ["runtimes"] = runtimes,
        }.ToJsonString();
    }

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
            var endpoint = NormalizeEndpoint(entry?["endpoint"] is JsonValue e && e.TryGetValue<string>(out var s) ? s : null) ?? DefaultEndpoint(id);
            return new LocalAiRuntime(id, enabled, endpoint);
        }).ToList();
        var changedBy = root["changedBy"] is JsonValue c && c.TryGetValue<string>(out var by) ? by : "";
        return new(true, TryLong(root["revision"]) ?? 0, changedBy, list);
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
