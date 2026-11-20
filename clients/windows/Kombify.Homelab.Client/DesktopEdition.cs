namespace Kombify.Homelab.Client;

// The edition is selected by the build; persisted configuration cannot turn a
// Local installation into a Cloud installation.
internal static class DesktopEdition
{
#if CLOUD_DESKTOP
    public const string Name = "cloud";
    public const string Channel = "beta";
    public const string UpdateManifestUrl = "https://github.com/kombifyio/Homelab/releases/download/windows-cloud-beta/kombify-homelab-windows-cloud-beta.json";
    public const string InstallerFileName = "kombify-Homelab-Cloud-Setup.exe";
    public const string VerifierFileName = "kombify-homelab-cloud-updater.exe";
    public const string InstallDirectoryName = "homelab-cloud";
    // The edition's own URL scheme for open-in-app (kombify-homelab-<variant>, AppLink v2).
    public const string ActivationScheme = "kombify-homelab-cloud";
    public static bool IsLocal => false;
#else
    public const string Name = "local";
    public const string Channel = "stable";
    public const string UpdateManifestUrl = "https://github.com/kombifyio/Homelab/releases/latest/download/kombify-homelab-windows-update.json";
    public const string InstallerFileName = "kombify-Homelab-Setup.exe";
    public const string VerifierFileName = "homelab.exe";
    public const string InstallDirectoryName = "homelab";
    public const string ActivationScheme = "kombify-homelab";
    public static bool IsLocal => true;
#endif
}
