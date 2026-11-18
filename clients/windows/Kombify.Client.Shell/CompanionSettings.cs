using System.Text.Json;
using System.Text.RegularExpressions;

namespace Kombify.Client.Shell;

// The Companion's settings snapshot of AppLink v2 ("Read: settings.get"):
// parsed strictly from the host's answer and written back in the same shape for
// a client's settings page (ToJson). Every field is always present; arrays may
// be empty. A client renders only the values it receives, in the order given.

/// <summary>The value types of the contract.</summary>
public static class CompanionSettingType
{
    public const string Bool = "bool";
    public const string Enum = "enum";
    public const string Chord = "chord";
    public const string Path = "path";
}

/// <summary>Keys a client handles itself; every other key is written as received.</summary>
public static class CompanionSettingKey
{
    /// <summary>The shared switch (v1 <c>Enabled</c>).</summary>
    public const string Enabled = "enabled";
    /// <summary>The screenshot folder: the client shows its own folder picker and writes an absolute local path.</summary>
    public const string ScreenshotFolder = "screenshot.folder";
}

/// <summary>
/// One setting the person can change. <see cref="Value"/> and <see cref="Default"/> are a
/// <see cref="bool"/> for <c>bool</c> and a <see cref="string"/> for <c>enum</c>, <c>chord</c>
/// and <c>path</c>. <see cref="Allowed"/> is set only for <c>enum</c>.
/// </summary>
public sealed record CompanionSettingValue(string Key, string Type, object Value, object Default, IReadOnlyList<string>? Allowed);

public sealed record CompanionMicrophone(string Id, string Label, bool IsDefault);

/// <summary>A chord that cannot be bound now. <see cref="With"/> is another key, or <c>windows</c>.</summary>
public sealed record CompanionShortcutConflict(string Key, string With);

/// <summary>A declared pill action; <see cref="Allowed"/> is false when the account may not use it.</summary>
public sealed record CompanionPillAction(string Id, bool Allowed);

public sealed record CompanionVoiceAnimation(string Id, bool Default);

/// <summary>The device's one kombify sign-in (AppLink v2 "Session"). <see cref="Label"/> is empty when not signed in; <see cref="Flag"/> is on, off or unknown.</summary>
public sealed record CompanionSession(bool SignedIn, string Label, string Flag, bool Consent);

/// <summary>
/// The end of one <c>settings.set</c>: the snapshot after the change, or a refusal code
/// (<see cref="CompanionAppLinkError"/>) with <see cref="With"/> for the two shortcut refusals.
/// On a refusal nothing changed.
/// </summary>
public sealed record CompanionSettingResult(CompanionSettings? Settings, string? Refusal = null, string? With = null)
{
    public bool Applied => Settings is not null;
}

public sealed partial record CompanionSettings
{
    internal const int MaxValueChars = 4096;
    private const int MaxItems = 256;

    public required ulong Revision { get; init; }
    public required bool Installed { get; init; }
    public required bool Running { get; init; }
    public required bool Enabled { get; init; }
    /// <summary>The program's version; empty while it is not running.</summary>
    public required string Version { get; init; }
    public required IReadOnlyList<CompanionSettingValue> Values { get; init; }
    public required IReadOnlyList<CompanionMicrophone> Microphones { get; init; }
    public required IReadOnlyList<CompanionShortcutConflict> Conflicts { get; init; }
    public required IReadOnlyList<CompanionPillAction> Pill { get; init; }
    public required IReadOnlyList<CompanionVoiceAnimation> VoiceAnimations { get; init; }
    public required IReadOnlyList<string> ScreenshotDestinations { get; init; }
    public required CompanionSession Session { get; init; }
    /// <summary>Account settings on the web (name to https URL), opened in the system browser.</summary>
    public required IReadOnlyList<KeyValuePair<string, string>> Links { get; init; }

    public CompanionStatus Status => new(Installed, Running, Enabled);

    public CompanionSettingValue? Find(string key) => Values.FirstOrDefault(v => v.Key == key);

    /// <summary>
    /// The snapshot while no program answers: revision 0, the device preference as
    /// <c>enabled</c> and only the <c>enabled</c> value, so a client shows just the switch.
    /// </summary>
    internal static CompanionSettings Stopped(bool installed, bool enabled) => new()
    {
        Revision = 0,
        Installed = installed,
        Running = false,
        Enabled = enabled,
        Version = "",
        Values = [new CompanionSettingValue(CompanionSettingKey.Enabled, CompanionSettingType.Bool, enabled, false, null)],
        Microphones = [],
        Conflicts = [],
        Pill = [],
        VoiceAnimations = [],
        ScreenshotDestinations = [],
        Session = new CompanionSession(false, "", "unknown", false),
        Links = [],
    };

    /// <summary>The snapshot in the contract's JSON shape (UTF-8 text), for a client's settings page.</summary>
    public string ToJson()
    {
        var output = new System.Buffers.ArrayBufferWriter<byte>();
        using (var w = new Utf8JsonWriter(output))
        {
            w.WriteStartObject();
            w.WriteNumber("revision", Revision);
            w.WriteBoolean("installed", Installed);
            w.WriteBoolean("running", Running);
            w.WriteBoolean("enabled", Enabled);
            w.WriteString("version", Version);
            w.WriteStartArray("values");
            foreach (var value in Values)
            {
                w.WriteStartObject();
                w.WriteString("key", value.Key);
                w.WriteString("type", value.Type);
                Scalar(w, "value", value.Value);
                Scalar(w, "default", value.Default);
                if (value.Allowed is { } allowed)
                {
                    w.WriteStartArray("allowed");
                    foreach (var item in allowed) w.WriteStringValue(item);
                    w.WriteEndArray();
                }
                w.WriteEndObject();
            }
            w.WriteEndArray();
            w.WriteStartArray("microphones");
            foreach (var m in Microphones)
            {
                w.WriteStartObject();
                w.WriteString("id", m.Id);
                w.WriteString("label", m.Label);
                w.WriteBoolean("isDefault", m.IsDefault);
                w.WriteEndObject();
            }
            w.WriteEndArray();
            w.WriteStartArray("conflicts");
            foreach (var c in Conflicts)
            {
                w.WriteStartObject();
                w.WriteString("key", c.Key);
                w.WriteString("with", c.With);
                w.WriteEndObject();
            }
            w.WriteEndArray();
            w.WriteStartArray("pill");
            foreach (var p in Pill)
            {
                w.WriteStartObject();
                w.WriteString("id", p.Id);
                w.WriteBoolean("allowed", p.Allowed);
                w.WriteEndObject();
            }
            w.WriteEndArray();
            w.WriteStartArray("voiceAnimations");
            foreach (var a in VoiceAnimations)
            {
                w.WriteStartObject();
                w.WriteString("id", a.Id);
                w.WriteBoolean("default", a.Default);
                w.WriteEndObject();
            }
            w.WriteEndArray();
            w.WriteStartArray("screenshotDestinations");
            foreach (var d in ScreenshotDestinations) w.WriteStringValue(d);
            w.WriteEndArray();
            w.WriteStartObject("session");
            w.WriteBoolean("signedIn", Session.SignedIn);
            w.WriteString("label", Session.Label);
            w.WriteString("flag", Session.Flag);
            w.WriteBoolean("consent", Session.Consent);
            w.WriteEndObject();
            w.WriteStartObject("links");
            foreach (var (name, url) in Links) w.WriteString(name, url);
            w.WriteEndObject();
            w.WriteEndObject();
        }
        return System.Text.Encoding.UTF8.GetString(output.WrittenSpan);
    }

    private static void Scalar(Utf8JsonWriter w, string name, object value)
    {
        if (value is bool flag) w.WriteBoolean(name, flag);
        else w.WriteString(name, (string)value);
    }

    // ── Strict reading (the structural pass already refused duplicates, null and depth) ──

    // Exactly the tokens of the program's chord parser (kombify-AI-Platform speechkit/app/internal/hotkeycombo,
    // ParseStrict) in the contract's canonical lower case without spaces: named keys, f1 to f24, one letter or digit.
    [GeneratedRegex(@"\A(?:(?:alt|ctrl|control|shift|win|windows|cmd|meta|space|enter|return|esc|escape|tab|backspace|mouse-x1|mouse-x2|mb4|mb5|f(?:[1-9]|1[0-9]|2[0-4])|[a-z0-9])(?:\+(?:alt|ctrl|control|shift|win|windows|cmd|meta|space|enter|return|esc|escape|tab|backspace|mouse-x1|mouse-x2|mb4|mb5|f(?:[1-9]|1[0-9]|2[0-4])|[a-z0-9])){0,15})?\z", RegexOptions.CultureInvariant)]
    private static partial Regex ChordPattern();

    [GeneratedRegex(@"\A(?:0|[1-9][0-9]{0,19})\z", RegexOptions.CultureInvariant)]
    private static partial Regex UnsignedPattern();

    private static CompanionAppLinkException Invalid() => CompanionAppLink.Fail(CompanionAppLinkError.InvalidResponse);

    /// <summary>Printable text: no C0 or C1 control character, at most <paramref name="max"/> characters.</summary>
    internal static bool ValidText(string? value, int max)
    {
        if (value is null || value.Length > max) return false;
        foreach (var c in value) if (c < 0x20 || c is >= '\u007f' and <= '\u009f') return false;
        return true;
    }

    internal static ulong ParseRevision(JsonElement element)
    {
        if (element.ValueKind != JsonValueKind.Number || !UnsignedPattern().IsMatch(element.GetRawText())
            || !ulong.TryParse(element.GetRawText(), System.Globalization.NumberStyles.None,
                System.Globalization.CultureInfo.InvariantCulture, out var revision))
            throw Invalid();
        return revision;
    }

    internal static CompanionSettings Parse(JsonElement root)
    {
        ulong? revision = null;
        bool? installed = null, running = null, enabled = null;
        string? version = null;
        List<CompanionSettingValue>? values = null;
        List<CompanionMicrophone>? microphones = null;
        List<CompanionShortcutConflict>? conflicts = null;
        List<CompanionPillAction>? pill = null;
        List<CompanionVoiceAnimation>? animations = null;
        List<string>? destinations = null;
        CompanionSession? session = null;
        List<KeyValuePair<string, string>>? links = null;
        foreach (var property in root.EnumerateObject())
        {
            var v = property.Value;
            switch (property.Name)
            {
                case "revision": revision = ParseRevision(v); break;
                case "installed": installed = Bool(v); break;
                case "running": running = Bool(v); break;
                case "enabled": enabled = Bool(v); break;
                case "version": version = Text(v, 64); break;
                case "values": values = Items(v, ParseValue); break;
                case "microphones":
                    microphones = Items(v, m =>
                    {
                        var f = Fields(m, "id", "label", "isDefault");
                        return new CompanionMicrophone(Text(f[0], 512, nonEmpty: true), Text(f[1], 256), Bool(f[2]));
                    });
                    break;
                case "conflicts":
                    conflicts = Items(v, c =>
                    {
                        var f = Fields(c, "key", "with");
                        return new CompanionShortcutConflict(Code(f[0]), Code(f[1]));
                    });
                    break;
                case "pill":
                    pill = Items(v, p =>
                    {
                        var f = Fields(p, "id", "allowed");
                        return new CompanionPillAction(Code(f[0]), Bool(f[1]));
                    });
                    break;
                case "voiceAnimations":
                    animations = Items(v, a =>
                    {
                        var f = Fields(a, "id", "default");
                        return new CompanionVoiceAnimation(Code(f[0]), Bool(f[1]));
                    });
                    break;
                case "screenshotDestinations": destinations = Items(v, Code); break;
                case "session": session = ParseSession(v); break;
                case "links": links = ParseLinks(v); break;
                default: throw Invalid();
            }
        }
        if (revision is null || installed is null || running is null || enabled is null || version is null || values is null
            || microphones is null || conflicts is null || pill is null || animations is null || destinations is null
            || session is null || links is null)
            throw Invalid();
        if (values.Select(x => x.Key).Distinct(StringComparer.Ordinal).Count() != values.Count
            || pill.Select(x => x.Id).Distinct(StringComparer.Ordinal).Count() != pill.Count
            || microphones.Select(x => x.Id).Distinct(StringComparer.Ordinal).Count() != microphones.Count)
            throw Invalid();
        return new CompanionSettings
        {
            Revision = revision.Value,
            Installed = installed.Value,
            Running = running.Value,
            Enabled = enabled.Value,
            Version = version,
            Values = values,
            Microphones = microphones,
            Conflicts = conflicts,
            Pill = pill,
            VoiceAnimations = animations,
            ScreenshotDestinations = destinations,
            Session = session,
            Links = links,
        };
    }

    private static CompanionSettingValue ParseValue(JsonElement element)
    {
        if (element.ValueKind != JsonValueKind.Object) throw Invalid();
        string? key = null, type = null;
        JsonElement? value = null, fallback = null;
        List<string>? allowed = null;
        foreach (var property in element.EnumerateObject())
        {
            switch (property.Name)
            {
                case "key": key = Code(property.Value); break;
                case "type": type = Code(property.Value); break;
                case "value": value = property.Value; break;
                case "default": fallback = property.Value; break;
                case "allowed": allowed = Items(property.Value, e => Text(e, 512)); break;
                default: throw Invalid();
            }
        }
        if (key is null || type is null || value is null || fallback is null) throw Invalid();
        if ((type == CompanionSettingType.Enum) != (allowed is not null)) throw Invalid();
        object current, initial;
        switch (type)
        {
            case CompanionSettingType.Bool:
                current = Bool(value.Value);
                initial = Bool(fallback.Value);
                break;
            case CompanionSettingType.Enum:
                current = Text(value.Value, 512);
                initial = Text(fallback.Value, 512);
                if (allowed!.Count == 0 || allowed.Distinct(StringComparer.Ordinal).Count() != allowed.Count
                    || !allowed.Contains((string)current, StringComparer.Ordinal))
                    throw Invalid();
                break;
            case CompanionSettingType.Chord:
                current = Chord(value.Value);
                initial = Chord(fallback.Value);
                break;
            case CompanionSettingType.Path:
                current = Text(value.Value, 32767);
                initial = Text(fallback.Value, 32767);
                break;
            default:
                throw Invalid(); // a type this client cannot render or write
        }
        return new CompanionSettingValue(key, type, current, initial, allowed);
    }

    private static CompanionSession ParseSession(JsonElement element)
    {
        if (element.ValueKind != JsonValueKind.Object) throw Invalid();
        bool? signedIn = null, consent = null;
        string? label = null, flag = null;
        foreach (var property in element.EnumerateObject())
        {
            switch (property.Name)
            {
                case "signedIn": signedIn = Bool(property.Value); break;
                case "label": label = Text(property.Value, 256); break;
                case "flag":
                    flag = Text(property.Value, 16);
                    if (flag is not ("on" or "off" or "unknown")) throw Invalid();
                    break;
                case "consent": consent = Bool(property.Value); break;
                default: throw Invalid();
            }
        }
        if (signedIn is null || label is null || flag is null || consent is null) throw Invalid();
        if (!signedIn.Value && label.Length != 0) throw Invalid();
        return new CompanionSession(signedIn.Value, label, flag, consent.Value);
    }

    /// <summary>Names are codes; every value is an absolute https URL without user information.</summary>
    private static List<KeyValuePair<string, string>> ParseLinks(JsonElement element)
    {
        if (element.ValueKind != JsonValueKind.Object) throw Invalid();
        var links = new List<KeyValuePair<string, string>>();
        foreach (var property in element.EnumerateObject())
        {
            if (!CompanionAppLink.ValidCode(property.Name) || links.Count >= 16) throw Invalid();
            var url = Text(property.Value, 2048, nonEmpty: true);
            if (!Uri.TryCreate(url, UriKind.Absolute, out var uri) || uri.Scheme != Uri.UriSchemeHttps
                || uri.UserInfo.Length != 0 || uri.HostNameType != UriHostNameType.Dns)
                throw Invalid();
            links.Add(new(property.Name, url));
        }
        return links;
    }

    private static List<T> Items<T>(JsonElement element, Func<JsonElement, T> item)
    {
        if (element.ValueKind != JsonValueKind.Array || element.GetArrayLength() > MaxItems) throw Invalid();
        return element.EnumerateArray().Select(item).ToList();
    }

    /// <summary>Exactly the named properties of an object, each once, in the order asked.</summary>
    private static JsonElement[] Fields(JsonElement element, params string[] names)
    {
        if (element.ValueKind != JsonValueKind.Object) throw Invalid();
        var found = new JsonElement?[names.Length];
        foreach (var property in element.EnumerateObject())
        {
            var index = Array.IndexOf(names, property.Name);
            if (index < 0) throw Invalid();
            found[index] = property.Value;
        }
        return found.Select(f => f ?? throw Invalid()).ToArray();
    }

    private static bool Bool(JsonElement element) => element.ValueKind switch
    {
        JsonValueKind.True => true,
        JsonValueKind.False => false,
        _ => throw Invalid(),
    };

    private static string Text(JsonElement element, int max) => Text(element, max, nonEmpty: false);

    private static string Text(JsonElement element, int max, bool nonEmpty)
    {
        if (element.ValueKind != JsonValueKind.String) throw Invalid();
        var text = element.GetString()!;
        if (!ValidText(text, max) || (nonEmpty && text.Length == 0)) throw Invalid();
        return text;
    }

    private static string Code(JsonElement element)
    {
        if (element.ValueKind != JsonValueKind.String || !CompanionAppLink.ValidCode(element.GetString())) throw Invalid();
        return element.GetString()!;
    }

    private static string Chord(JsonElement element)
    {
        if (element.ValueKind != JsonValueKind.String || !ChordPattern().IsMatch(element.GetString()!)) throw Invalid();
        return element.GetString()!;
    }
}
