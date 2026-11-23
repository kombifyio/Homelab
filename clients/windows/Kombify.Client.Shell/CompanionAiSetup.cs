using System.Text.Json;
using System.Text.Json.Nodes;
using System.Text.RegularExpressions;

namespace Kombify.Client.Shell;

// Which AI the person had set in an app when they report a fault or turn to support: the content-free
// `aiSetup` block of SurfaceContextV2 (workspace FLOATING-PANEL-V2-CONTRACT §7, AppLink v2
// `context.report`, platform/standards/companion-applink.v2.md). Nothing here carries an endpoint
// URL, a prompt or a key; `provider` and `model` are short codes (driver id, model id) only.
//
// The same block feeds three places: the context an app sends with every Companion panel turn and
// every AppLink report (`ToJson`), and the error-report tags and diagnostics bundle of the app (`Tags`).

/// <summary>
/// The app's AI setup. <see cref="AccountAiUse"/> is the account choice (<c>kombify|own|none|unknown</c>:
/// <c>undecided</c> or an unreadable record is <c>unknown</c>); <see cref="ActiveSource"/> is what answers
/// in this app right now (<c>kombify|other|local</c>).
/// </summary>
public sealed partial record CompanionAiSetup
{
    public const string SourceKombify = "kombify";
    public const string SourceOther = "other";
    public const string SourceLocal = "local";

    public required string AccountAiUse { get; init; }
    public required string ActiveSource { get; init; }
    public string? Provider { get; init; }
    public string? Model { get; init; }
    public CompanionLocalRuntimes? LocalRuntimes { get; init; }

    /// <summary>
    /// The local runtimes as the device-wide file holds them, by row id (the device's <c>ollama</c> and
    /// runtimes on another machine such as <c>ollama-nas</c>, at most <see cref="MaxLocalRuntimes"/>):
    /// <c>enabled</c> from the file; <c>reachable</c> only when <paramref name="statuses"/> has a status
    /// for it (a probe was made).
    /// </summary>
    public static CompanionLocalRuntimes LocalFrom(LocalAiRuntimesSnapshot snapshot,
        IReadOnlyDictionary<string, LocalAiRuntimeStatus>? statuses = null)
    {
        var runtimes = new Dictionary<string, CompanionLocalRuntime>(StringComparer.Ordinal);
        foreach (var runtime in snapshot.Runtimes.Take(MaxLocalRuntimes))
        {
            bool? reachable = statuses is not null && statuses.TryGetValue(runtime.Id, out var status) ? status.Reachable : null;
            runtimes[runtime.Id] = new CompanionLocalRuntime { Enabled = runtime.Enabled, Reachable = reachable };
        }
        return new CompanionLocalRuntimes { Runtimes = runtimes };
    }

    /// <summary>The most runtimes one block names (the schema's <c>maxProperties</c>).</summary>
    public const int MaxLocalRuntimes = 8;

    /// <summary>
    /// Strict: exactly the wire shape, or null (an unknown or null member, a wrong type, an unknown
    /// enum value, a code outside <c>^[A-Za-z0-9._:@/+\-]{1,160}$</c>, an empty runtimes entry).
    /// </summary>
    public static CompanionAiSetup? Parse(JsonElement element)
    {
        if (element.ValueKind != JsonValueKind.Object) return null;
        string? account = null, source = null, provider = null, model = null;
        CompanionLocalRuntimes? local = null;
        var seen = new HashSet<string>(StringComparer.Ordinal);
        foreach (var property in element.EnumerateObject())
        {
            if (!seen.Add(property.Name)) return null;
            var value = property.Value;
            switch (property.Name)
            {
                case "accountAiUse":
                    if (value.ValueKind != JsonValueKind.String) return null;
                    account = value.GetString();
                    break;
                case "activeSource":
                    if (value.ValueKind != JsonValueKind.String) return null;
                    source = value.GetString();
                    break;
                case "provider":
                    if (value.ValueKind != JsonValueKind.String) return null;
                    provider = value.GetString();
                    break;
                case "model":
                    if (value.ValueKind != JsonValueKind.String) return null;
                    model = value.GetString();
                    break;
                case "localRuntimes":
                    if (ParseLocal(value) is not { } parsed) return null;
                    local = parsed;
                    break;
                default:
                    return null;
            }
        }
        // A present code must match the pattern: an empty string is a wrong shape here, not "absent".
        if (account is null || source is null
            || (provider is not null && !CompanionAppLink.ValidCode(provider))
            || (model is not null && !CompanionAppLink.ValidCode(model))) return null;
        var setup = new CompanionAiSetup
        {
            AccountAiUse = account, ActiveSource = source, Provider = provider, Model = model, LocalRuntimes = local,
        };
        return setup.Valid() ? setup : null;
    }

    /// <summary>
    /// The error-report tags: <c>ai.source</c>, <c>ai.account_use</c>, <c>ai.provider</c> (omitted when
    /// absent) and <c>ai.local.ollama</c> = <c>off</c> | <c>on</c> (enabled, reachability unknown) |
    /// <c>reachable</c> | <c>unreachable</c> (omitted while the local runtimes are unknown).
    /// </summary>
    public IReadOnlyDictionary<string, string> Tags()
    {
        var tags = new Dictionary<string, string>(StringComparer.Ordinal)
        {
            ["ai.source"] = ActiveSource,
            ["ai.account_use"] = AccountAiUse,
        };
        if (!string.IsNullOrEmpty(Provider)) tags["ai.provider"] = Provider;
        if (LocalRuntimes?.Ollama is { } ollama)
            tags["ai.local.ollama"] = !ollama.Enabled ? "off" : ollama.Reachable switch
            {
                true => "reachable",
                false => "unreachable",
                null => "on",
            };
        return tags;
    }

    /// <summary>The block in the wire shape; absent members are omitted, never null.</summary>
    public JsonObject ToJson()
    {
        var block = new JsonObject
        {
            ["accountAiUse"] = AccountAiUse,
            ["activeSource"] = ActiveSource,
        };
        if (!string.IsNullOrEmpty(Provider)) block["provider"] = Provider;
        if (!string.IsNullOrEmpty(Model)) block["model"] = Model;
        if (LocalRuntimes is not null)
        {
            var runtimes = new JsonObject();
            foreach (var (id, local) in LocalRuntimes.Runtimes)
            {
                var runtime = new JsonObject { ["enabled"] = local.Enabled };
                if (local.Reachable is { } reachable) runtime["reachable"] = reachable;
                runtimes[id] = runtime;
            }
            block["localRuntimes"] = runtimes;
        }
        return block;
    }

    /// <summary>The wire rules: closed enums, optional short codes (an empty string reads as absent).</summary>
    internal bool Valid() =>
        AccountAiUse is "kombify" or "own" or "none" or "unknown"
        && ActiveSource is SourceKombify or SourceOther or SourceLocal
        && OptionalCode(Provider) && OptionalCode(Model);

    private static bool OptionalCode(string? value) => string.IsNullOrEmpty(value) || CompanionAppLink.ValidCode(value);

    private static CompanionLocalRuntimes? ParseLocal(JsonElement element)
    {
        if (element.ValueKind != JsonValueKind.Object) return null;
        var runtimes = new Dictionary<string, CompanionLocalRuntime>(StringComparer.Ordinal);
        foreach (var property in element.EnumerateObject())
        {
            if (runtimes.ContainsKey(property.Name) || runtimes.Count >= MaxLocalRuntimes
                || !RuntimeId().IsMatch(property.Name) || property.Value.ValueKind != JsonValueKind.Object) return null;
            bool? enabled = null, reachable = null;
            var inner = new HashSet<string>(StringComparer.Ordinal);
            foreach (var member in property.Value.EnumerateObject())
            {
                if (!inner.Add(member.Name)) return null;
                if (member.Value.ValueKind is not (JsonValueKind.True or JsonValueKind.False)) return null;
                switch (member.Name)
                {
                    case "enabled": enabled = member.Value.GetBoolean(); break;
                    case "reachable": reachable = member.Value.GetBoolean(); break;
                    default: return null;
                }
            }
            if (enabled is not { } on) return null;
            runtimes[property.Name] = new CompanionLocalRuntime { Enabled = on, Reachable = reachable };
        }
        return new CompanionLocalRuntimes { Runtimes = runtimes };
    }

    [GeneratedRegex("^[a-z][a-z0-9-]{0,31}$")]
    private static partial Regex RuntimeId();
}

/// <summary>The local runtimes of the block by row id; a runtime that is not named is not known.</summary>
public sealed record CompanionLocalRuntimes
{
    public IReadOnlyDictionary<string, CompanionLocalRuntime> Runtimes { get; init; } =
        new Dictionary<string, CompanionLocalRuntime>(StringComparer.Ordinal);

    /// <summary>The device's own Ollama, when named.</summary>
    public CompanionLocalRuntime? Ollama => Runtimes.GetValueOrDefault(LocalAiRuntimes.Ollama);
}

/// <summary><see cref="Reachable"/> is set only when a probe was made.</summary>
public sealed record CompanionLocalRuntime
{
    public required bool Enabled { get; init; }
    public bool? Reachable { get; init; }
}
