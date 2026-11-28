using System.Globalization;
using System.Resources;
using Microsoft.Win32;

namespace Kombify.Homelab.Client;

// The words of the shell's own pages (start, failure, update) in the Windows display language.
internal static class ShellText
{
    // The tool's name in the window title and on its own pages (D-2026-11-20-04).
    internal const string ProductName = "Homelab Runtime";

    private static readonly ResourceManager Resources = new(
        "Kombify.Homelab.Client.ShellMessages", typeof(ShellText).Assembly);

    internal static string Get(string key) => Resources.GetString(key, CultureInfo.CurrentUICulture)
        ?? throw new InvalidOperationException("Missing shell message: " + key);

    // Windows' app theme (Settings > Personalization > Colors), so the window behind the page matches it.
    internal static bool PrefersDark()
    {
        try
        {
            return Registry.GetValue(@"HKEY_CURRENT_USER\Software\Microsoft\Windows\CurrentVersion\Themes\Personalize",
                "AppsUseLightTheme", 1) is int light && light == 0;
        }
        catch (Exception error) when (error is System.Security.SecurityException or IOException or UnauthorizedAccessException)
        {
            return false;
        }
    }
}
