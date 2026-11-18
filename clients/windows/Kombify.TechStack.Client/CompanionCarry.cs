using System.Reflection;
using System.Text.Json;
using System.Text.RegularExpressions;
using Kombify.Client.Shell;

namespace Kombify.TechStack.Client;

// The desktop Companion installer this client carries beside itself (companion\<file>), pinned in
// companion-carry.json: version, size and sha256 in one place, embedded here and read by the build
// scripts. Nothing is downloaded at run time. A build without the file (the public Standalone
// export, a development build) carries no Companion and then has no Companion tab unless one is
// already installed on the machine.
internal static partial class CompanionCarry
{
    private const string ResourceName = "companion-carry.json";

    [GeneratedRegex(@"\A[A-Za-z0-9._-]{1,120}\.msi\z", RegexOptions.CultureInvariant)]
    private static partial Regex FilePattern();

    [GeneratedRegex(@"\A(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\z", RegexOptions.CultureInvariant)]
    private static partial Regex VersionPattern();

    [GeneratedRegex(@"\A[0-9a-f]{64}\z", RegexOptions.CultureInvariant)]
    private static partial Regex DigestPattern();

    private sealed record Pin(string Version, string File, long Size, string Sha256);

    private static readonly Lazy<Pin?> Parsed = new(ReadPin);

    private static Pin? ReadPin()
    {
        try
        {
            using var stream = Assembly.GetExecutingAssembly().GetManifestResourceStream(ResourceName);
            if (stream is null) return null;
            using var document = JsonDocument.Parse(stream);
            var root = document.RootElement;
            var pin = new Pin(
                root.GetProperty("version").GetString() ?? "",
                root.GetProperty("file").GetString() ?? "",
                root.GetProperty("size").GetInt64(),
                root.GetProperty("sha256").GetString() ?? "");
            return VersionPattern().IsMatch(pin.Version) && FilePattern().IsMatch(pin.File)
                && pin.Size > 0 && DigestPattern().IsMatch(pin.Sha256) ? pin : null;
        }
        catch (Exception error) when (error is JsonException or KeyNotFoundException or InvalidOperationException
            or FormatException or IOException)
        {
            return null;
        }
    }

    /// <summary>
    /// The carried payload, or null when this build carries none (no pin, or no file of the pinned
    /// size beside the executable). The digest is checked by <see cref="CompanionInstaller"/> itself.
    /// </summary>
    public static CompanionPayload? Payload()
    {
        if (Parsed.Value is not { } pin) return null;
        var path = Path.Combine(AppContext.BaseDirectory, "companion", pin.File);
        try
        {
            var file = new FileInfo(path);
            return file.Exists && file.Length == pin.Size ? new CompanionPayload(path, pin.Version, pin.Sha256) : null;
        }
        catch (Exception error) when (error is IOException or UnauthorizedAccessException)
        {
            return null;
        }
    }
}
