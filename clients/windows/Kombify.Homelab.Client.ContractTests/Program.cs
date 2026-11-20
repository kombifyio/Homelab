using Kombify.Homelab.Client;
using Kombify.Client.Shell;

// The process substitute belongs only to this test executable. Shipping clients
// retain the embedded-key Go verifier; no production trust override is added.
if (args.FirstOrDefault() == "client-update" &&
    Environment.GetEnvironmentVariable("KOMBIFY_TEST_UPDATE_VERIFIER_ROOT") is { } verifierRoot)
    return RunRecoveryVerifierProcessProbe(args, verifierRoot);

if (args.Contains("--cloud-updater-only", StringComparer.Ordinal))
{
    AssertCloudUpdateRetainsRecoveryCursor();
    AssertCloudRecoveryReacquiresMissingCache();
    AssertLegacyRollbackResumesWithVerifierEvidence();
    return 0;
}

if (args.Contains("--wallet-live-probe", StringComparer.Ordinal))
{
    if (!DesktopEdition.IsLocal) throw new InvalidOperationException("Wallet device proof belongs to the Standalone installer.");
    await WindowsLocalWalletDeviceRevealBoundary(args);
    return 0;
}

if (args.Contains("--handoff-live-probe", StringComparer.Ordinal))
{
    await ObserveInstallerHandoffAsync();
    return 0;
}

if (args.Contains("--rollback-only", StringComparer.Ordinal))
{
    if (!DesktopEdition.IsLocal) throw new InvalidOperationException("Rollback belongs to the Standalone installer.");
    AssertLegacyRollbackResumesWithVerifierEvidence();
    AssertFailedUpdatePreservesRecovery();
    AssertPendingRollbackResumesBeforeStartup();
    Console.WriteLine("PASS pending rollback protects and restores data before runtime startup");
    return 0;
}

// One report per assertion, and the run continues after a failure: every PASS and FAIL line of this edition appears in
// the one dispatch, and the exit code is non-zero only after all of them ran.
var failures = new List<string>();
var passed = 0;
string Summary() => $"{DesktopEdition.Name}: {passed} passed, {failures.Count} failed";
void Report(string name, Action check)
{
    try
    {
        check();
        passed++;
        Console.WriteLine($"PASS {name}");
    }
    catch (Exception error)
    {
        failures.Add(name);
        Console.WriteLine($"FAIL {name}: {error.Message}");
    }
}
async Task ReportAsync(string name, Func<Task> check)
{
    try
    {
        await check();
        passed++;
        Console.WriteLine($"PASS {name}");
    }
    catch (Exception error)
    {
        failures.Add(name);
        Console.WriteLine($"FAIL {name}: {error.Message}");
    }
}

Report($"{DesktopEdition.Name} installer authority", AssertInstallerAuthority);
Report("device context stays inside the current product document and navigation lifetime", AssertDeviceContextDocumentBoundary);
await ReportAsync("credential requests do not follow redirects", AssertCredentialRedirectRejected);
#if CLOUD_DESKTOP
Report("Cloud update restores the operator cache and retains its recovery cursor", AssertCloudUpdateRetainsRecoveryCursor);
Report("Cloud recovery reacquires a missing cache through the edition helper", AssertCloudRecoveryReacquiresMissingCache);
Report("legacy rollback resumes with verifier evidence", AssertLegacyRollbackResumesWithVerifierEvidence);
await ReportAsync("Connect rejects an account mismatch before custody or request effects", AssertConnectAccountMismatchDenied);
await ReportAsync("only a refused grant ends the Connect session; 429, 5xx, network faults and timeouts keep the stored token", AssertConnectRefreshFailureRule);
#endif
if (DesktopEdition.IsLocal)
{
    await ReportAsync("spoofed local runtime cannot capture the device token", AssertSpoofedLocalRuntimeRejected);
}
if (args.Contains("--authority-only", StringComparer.Ordinal))
{
    Console.WriteLine(Summary());
    return failures.Count == 0 ? 0 : 1;
}
if (DesktopEdition.IsLocal)
{
    Report("legacy rollback resumes with verifier evidence", AssertLegacyRollbackResumesWithVerifierEvidence);
    Report("a failed update preserves recovery", AssertFailedUpdatePreservesRecovery);
    Report("pending rollback protects and restores data before runtime startup", AssertPendingRollbackResumesBeforeStartup);
}

if (args.Contains("--credential-roundtrip", StringComparer.Ordinal))
{
    Report("Windows Credential Manager roundtrip", () =>
    {
        var target = $"kombify/homelab/contract-tests/{Guid.NewGuid():N}";
        var secret = Convert.ToHexString(System.Security.Cryptography.RandomNumberGenerator.GetBytes(32)).ToLowerInvariant();
        try
        {
            WindowsCredentialStore.Write(target, Environment.UserName, secret);
            if (!string.Equals(WindowsCredentialStore.Read(target), secret, StringComparison.Ordinal))
            {
                throw new InvalidOperationException("Windows Credential Manager roundtrip changed the secret.");
            }
        }
        finally
        {
            TryDeleteCredential(target);
        }
    });
}

// A cleanup failure must never replace the check's own result. The Credential Manager is absent on a logon without a
// user vault (a non-interactive SSH logon answers 1312, "no such logon session"), which says nothing about what the
// assertions observed; the line stays visible in the run output instead.
static void TryDeleteCredential(string target)
{
    try
    {
        WindowsCredentialStore.Delete(target);
    }
    catch (Exception error)
    {
        Console.WriteLine($"WARN credential cleanup left {target} in place: {error.GetType().Name}: {error.Message}");
    }
}

// Connect installation key: a DPoP proof must verify under the key whose RFC 7638
// thumbprint Gateway stores as dpop_jkt, and the key must not be exportable.
{
    string? custody = null;
    Report("Connect DPoP proof verifies under the installation key and the key is not exportable", () =>
    {
        var keyName = $"kombify-homelab-contract-tests-{Guid.NewGuid():N}";
        try
        {
            using var key = ConnectDeviceKey.OpenOrCreate(keyName);
            custody = key.Custody.ToString();
            var proof = key.CreateProof("POST", new Uri("https://connect.kombify.io/v1/installations?x=1"));
            var segments = proof.Split('.');
            var payload = System.Text.Json.JsonDocument.Parse(Convert.FromBase64String(
                segments[1].Replace('-', '+').Replace('_', '/').PadRight(segments[1].Length + (4 - segments[1].Length % 4) % 4, '=')));
            if (!key.VerifyProof(proof)
                || payload.RootElement.GetProperty("htu").GetString() != "https://connect.kombify.io/v1/installations"
                || key.Thumbprint.Length != 43)
            {
                throw new InvalidOperationException("Connect DPoP proof does not verify under the installation key.");
            }
        }
        finally
        {
            ConnectDeviceKey.Delete(keyName);
        }
    });
    if (custody is not null) Console.WriteLine($"INFO Connect DPoP key custody: {custody}");
}

const string ValidSelfHosted = """
{
  "version":"1",
  "deployment_mode":"self_hosted",
  "base_url":"https://home.example.net/",
  "instance_id":"homelab-home-001",
  "oidc":{"issuer":"https://home.example.net/oidc/","client_id":"windows-public","scopes":["openid","profile","offline_access"],"flow":"device_authorization"},
  "capabilities":["homelab.runtime.read","homelab.runtime.write"],
  "api_versions":{"homelab":"v1"},
  "sync":{"offline_read":false,"offline_write":"disabled","etag":false,"tombstones":false}
}
""";

const string ValidLocal = """
{
  "version":"1",
  "deployment_mode":"local",
  "base_url":"http://127.0.0.1:5260/",
  "instance_id":"homelab-local-001",
  "oidc":{"issuer":"http://127.0.0.1:5260/","client_id":"homelab-local","scopes":["openid","profile"],"flow":"local_bootstrap"},
  "capabilities":["homelab.runtime.read","homelab.runtime.write"],
  "api_versions":{"homelab":"v1"},
  "sync":{"offline_read":false,"offline_write":"disabled","etag":false,"tombstones":false}
}
""";

// Shared contracts hold in both editions; an edition's own contract runs only in that edition.
var sharedTests = new (string Name, Action Execute)[]
{
    ("self-hosted HTTPS profile", () => Parse(ValidSelfHosted, "https://home.example.net/")),
    ("local loopback profile", () => Parse(ValidLocal, "http://127.0.0.1:5260/")),
    ("remote HTTP rejected", () => MustFail(() => ClientConnectionProfileValidator.NormalizeConfiguredEndpoint("http://home.example.net/"))),
    ("mismatched base URL rejected", () => MustFail(() => Parse(ValidSelfHosted, "https://other.example.net/"))),
    ("unknown token field rejected", () => MustFail(() => Parse(ValidSelfHosted.Replace("\"sync\":", "\"access_token\":\"secret\",\"sync\":"), "https://home.example.net/"))),
    ("cloud device flow rejected", () => MustFail(() => Parse(ValidSelfHosted.Replace("\"self_hosted\"", "\"cloud\""), "https://home.example.net/"))),
    ("default credential target remains stable", AssertDefaultCredentialTarget),
};
// The Standalone installer owns the local device-session entry, the local state root and the refusal of every Cloud
// entry point; the Connected installer refuses any non-cloud mode instead (Homelab.Client/Program.cs:163-166).
var localTests = new (string Name, Action Execute)[]
{
    ("local startup opens the device-session entry without mode selection", AssertLocalStartupEntry),
    ("Standalone edition rejects Cloud discovery", AssertLocalEditionRejectsCloudProfile),
    ("isolated state receives stable credential namespace", AssertIsolatedCredentialNamespace),
    ("state root override falls back to product state", () => AssertRejectedStateOverride("")),
    ("state sibling override falls back to product state", () => AssertRejectedStateOverride("-outside")),
};

foreach (var test in sharedTests) Report(test.Name, test.Execute);
if (DesktopEdition.IsLocal)
{
    foreach (var test in localTests) Report(test.Name, test.Execute);
}

Console.WriteLine(Summary());
if (failures.Count != 0)
{
    Console.WriteLine($"{DesktopEdition.Name}: FAILED {string.Join(" | ", failures)}");
    return 1;
}
return 0;

// This development probe uses the real coordinator with owned harmless
// executables. It performs no installation, elevation or real state reset.
static async Task ObserveInstallerHandoffAsync()
{
    var root = Path.Combine(Environment.GetFolderPath(Environment.SpecialFolder.LocalApplicationData), "kombify");
    var state = Path.Combine(root, $"techstack-client-contract-{Guid.NewGuid():N}");
    Directory.CreateDirectory(state);
    System.Diagnostics.Process? coordinator = null;
    try
    {
        var source = Path.Combine(Environment.GetFolderPath(Environment.SpecialFolder.System), "cmd.exe");
        var first = Path.Combine(state, "first.exe");
        var second = Path.Combine(state, "second.exe");
        File.Copy(source, first);
        File.Copy(source, second);
        var digest = Convert.ToHexString(System.Security.Cryptography.SHA256.HashData(File.ReadAllBytes(source)));
        var size = new FileInfo(source).Length;
        var entered = Path.Combine(state, "entered");
        var continued = Path.Combine(state, "continued");
        var start = ClientUpdater.InstallerHandoffStartInfo([
            new ClientUpdater.InstallerHandoff(first, digest, size,
                "/d /c \"echo entered>\"%KOMBIFY_HANDOFF_PROBE_ENTERED%\" & ping -n 7 127.0.0.1 >nul & exit /b 23\""),
            new ClientUpdater.InstallerHandoff(second, digest, size,
                "/d /c \"echo continued>\"%KOMBIFY_HANDOFF_PROBE_CONTINUED%\" & exit /b 0\"")],
            Path.Combine(state, "never-restart.exe"), Path.Combine(state, "handoff.log"), 0);
        start.Environment["KOMBIFY_HANDOFF_PROBE_ENTERED"] = entered;
        start.Environment["KOMBIFY_HANDOFF_PROBE_CONTINUED"] = continued;
        coordinator = System.Diagnostics.Process.Start(start) ?? throw new InvalidOperationException("The handoff probe did not start.");
        var deadline = DateTime.UtcNow.AddSeconds(20);
        while (!File.Exists(entered) && !coordinator.HasExited && DateTime.UtcNow < deadline)
            await Task.Delay(50);
        if (!File.Exists(entered)) throw new InvalidOperationException("The harmless installer did not reach execution.");
        foreach (var path in new[] { first, second })
        {
            var writeDenied = false;
            var deleteDenied = false;
            try { File.WriteAllText(path, "substituted installer"); } catch (IOException) { writeDenied = true; }
            try { File.Delete(path); } catch (IOException) { deleteDenied = true; }
            if (!writeDenied || !deleteDenied)
                throw new InvalidOperationException("An installer could be replaced or deleted during handoff.");
        }
        using var budget = new CancellationTokenSource(TimeSpan.FromSeconds(20));
        await coordinator.WaitForExitAsync(budget.Token);
        if (coordinator.ExitCode == 0 || File.Exists(continued))
            throw new InvalidOperationException("A failed installer allowed continuation or restart.");
        Console.WriteLine("Observed installer handoff: both packages denied write/delete; failed installer stopped continuation and restart.");
    }
    finally
    {
        if (coordinator is not null)
        {
            if (!coordinator.HasExited) { coordinator.Kill(entireProcessTree: true); await coordinator.WaitForExitAsync(); }
            coordinator.Dispose();
        }
        if (!Path.GetFullPath(state).StartsWith(Path.GetFullPath(root) + Path.DirectorySeparatorChar, StringComparison.OrdinalIgnoreCase))
            throw new InvalidOperationException("The handoff probe escaped its owned state root.");
        Directory.Delete(state, recursive: true);
    }
}

// The Go verifier's cryptographic boundary is exercised independently. This
// module probe consumes its approved results and observes the durable migration
// and handoff, without launching any installer or supplying a runtime key.
static void AssertLegacyRollbackResumesWithVerifierEvidence()
{
    var previous = Environment.GetEnvironmentVariable("HOMELAB_CLIENT_STATE_DIR");
    var state = DesktopEdition.IsLocal
        ? Path.Combine(Environment.GetFolderPath(Environment.SpecialFolder.LocalApplicationData),
            "kombify", $"techstack-client-contract-{Guid.NewGuid():N}")
        : Path.Combine(new ClientStatePaths("techstack", "cloud", "beta").DefaultDirectory,
            $"contract-recovery-{Guid.NewGuid():N}");
    var listener = new System.Net.Sockets.TcpListener(System.Net.IPAddress.Loopback, 0);
    try
    {
        Environment.SetEnvironmentVariable("HOMELAB_CLIENT_STATE_DIR", state);
        listener.Start();
        var port = ((System.Net.IPEndPoint)listener.LocalEndpoint).Port;
        listener.Stop();
        var config = new ClientConfig { AutoUpdate = false, LocalUiUrl = $"http://127.0.0.1:{port}/" };
        var updates = ClientUpdater.UpdatesDirectory();
        var snapshot = Path.Combine(updates, "snapshot");
        Directory.CreateDirectory(snapshot);
        var originalData = System.Security.Cryptography.RandomNumberGenerator.GetBytes(32);
        File.WriteAllBytes(Path.Combine(snapshot, "operator-data"), originalData);
        Directory.CreateDirectory(config.RuntimeDataDir);
        var migratedData = System.Security.Cryptography.RandomNumberGenerator.GetBytes(32);
        File.WriteAllBytes(Path.Combine(config.RuntimeDataDir, "operator-data"), migratedData);
        var pending = Path.Combine(updates, "pending.json");
        var untrusted = Path.Combine(state, "legacy-untrusted.exe");
        var untrustedBytes = System.Security.Cryptography.RandomNumberGenerator.GetBytes(32);
        File.WriteAllBytes(untrusted, untrustedBytes);
        File.WriteAllText(pending, System.Text.Json.JsonSerializer.Serialize(new
        {
            from = "0.0.0-0", to = ClientUpdater.CurrentVersion(), phase = "rolling-back",
            installer = untrusted, rollbackInstaller = untrusted,
        }));
        var oldCursor = File.ReadAllBytes(pending);
        var approved = false;
        var verifierReached = false;
        var launched = false;
        ClientUpdater.VerifiedPackage Resolve(ClientConfig _, string root, string version)
        {
            verifierReached = true;
            if (!approved) throw new InvalidDataException("The historical signed manifest is not available yet.");
            var directory = Path.Combine(root, version);
            Directory.CreateDirectory(directory);
            var path = Path.Combine(directory, "package");
            var bytes = System.Text.Encoding.UTF8.GetBytes("verifier-approved release " + version);
            File.WriteAllBytes(path, bytes);
            return new ClientUpdater.VerifiedPackage { Status = "verified-rollback", Version = version,
                PackagePath = path, PackageSize = bytes.Length,
                PackageSha256 = Convert.ToHexString(System.Security.Cryptography.SHA256.HashData(bytes)) };
        }
        void Launch(ClientUpdater.InstallerHandoff[] installers)
        {
            launched = true;
            using var cursor = System.Text.Json.JsonDocument.Parse(File.ReadAllBytes(pending));
            foreach (var installer in installers)
            {
                if (Path.GetFullPath(installer.Path).Equals(Path.GetFullPath(untrusted), StringComparison.OrdinalIgnoreCase))
                    throw new InvalidOperationException("Legacy paths authorized the resumed handoff.");
                var bytes = File.ReadAllBytes(installer.Path);
                var hash = Convert.ToHexString(System.Security.Cryptography.SHA256.HashData(bytes));
                var isRecovery = installer.Arguments == "/passive /norestart";
                var storedHash = cursor.RootElement.GetProperty(isRecovery ? "rollbackSha256" : "installerSha256").GetString();
                var storedPath = cursor.RootElement.GetProperty(isRecovery ? "rollbackInstaller" : "installer").GetString();
                if (!hash.Equals(installer.Sha256, StringComparison.OrdinalIgnoreCase) || bytes.Length != installer.Size ||
                    !hash.Equals(storedHash, StringComparison.OrdinalIgnoreCase) || storedPath != installer.Path)
                    throw new InvalidOperationException("Installer handoff preceded durable verifier-derived migration.");
            }
        }
        if (ClientUpdater.ResumePendingRollback(config, out var blocked, Resolve, Launch) || blocked is null || launched ||
            !File.ReadAllBytes(pending).SequenceEqual(oldCursor))
            throw new InvalidOperationException("Unavailable signatures lost or authorized legacy recovery.");
        listener = new System.Net.Sockets.TcpListener(System.Net.IPAddress.Loopback, port);
        listener.Start();
        approved = true;
        verifierReached = false;
        var resumedWithLocalPort = ClientUpdater.ResumePendingRollback(config, out blocked, Resolve, Launch);
        if (DesktopEdition.IsLocal)
        {
            if (resumedWithLocalPort || blocked is null || verifierReached || launched)
                throw new InvalidOperationException("Local recovery crossed the running-runtime boundary.");
            listener.Stop();
            if (!ClientUpdater.ResumePendingRollback(config, out blocked, Resolve, Launch) || blocked is not null || !launched)
                throw new InvalidOperationException("Available verifier evidence did not resume Local recovery.");
        }
        else if (!resumedWithLocalPort || blocked is not null || !verifierReached || !launched)
            throw new InvalidOperationException("A coexisting Local listener blocked Cloud recovery.");
        if (!File.ReadAllBytes(untrusted).SequenceEqual(untrustedBytes) ||
            !File.ReadAllBytes(Path.Combine(snapshot, "operator-data")).SequenceEqual(originalData) ||
            !File.ReadAllBytes(Path.Combine(config.RuntimeDataDir, "operator-data")).SequenceEqual(migratedData))
            throw new InvalidOperationException("Artifact migration changed retained data before installation recovery.");
        Console.WriteLine($"Observed {DesktopEdition.Name} rollback: missing signatures retained recovery; approved packages durably bound before handoff; occupied Local endpoint=127.0.0.1:{port}; Local listener protection={DesktopEdition.IsLocal}.");
    }
    finally
    {
        listener.Stop();
        Environment.SetEnvironmentVariable("HOMELAB_CLIENT_STATE_DIR", previous);
        if (Directory.Exists(state)) Directory.Delete(state, recursive: true);
    }
}

static int RunRecoveryVerifierProcessProbe(string[] arguments, string fixtureRoot)
{
    string Value(string flag) => arguments[Array.IndexOf(arguments, flag) + 1];
    var root = Path.GetFullPath(Value("--staging-dir"));
    if (!root.StartsWith(Path.GetFullPath(fixtureRoot) + Path.DirectorySeparatorChar, StringComparison.OrdinalIgnoreCase))
        throw new InvalidOperationException("The verifier probe escaped its owned state.");
    var staging = arguments.Contains("--rollback-version", StringComparer.Ordinal);
    var version = Value(staging ? "--rollback-version" : "--verify-staged-version");
    if (Path.GetFileName(version) != version) throw new InvalidDataException("Unsafe probe version.");
    var package = Path.Combine(root, version, "package");
    if (staging)
    {
        Directory.CreateDirectory(Path.GetDirectoryName(package)!);
        File.Copy(Path.Combine(fixtureRoot, "approved-package"), package);
        Console.WriteLine("{\"status\":\"rollback-staged\"}");
        return 0;
    }
    if (!File.Exists(package)) return 1;
    var bytes = File.ReadAllBytes(package);
    Console.WriteLine(System.Text.Json.JsonSerializer.Serialize(new
    {
        status = "verified-rollback", version, package_path = package,
        package_sha256 = Convert.ToHexString(System.Security.Cryptography.SHA256.HashData(bytes)),
        package_size = bytes.Length,
    }));
    return 0;
}

static void AssertCloudRecoveryReacquiresMissingCache()
{
    if (DesktopEdition.IsLocal) throw new InvalidOperationException("Cloud recovery proof requires the Cloud build.");
    var previousState = Environment.GetEnvironmentVariable("HOMELAB_CLIENT_STATE_DIR");
    var previousProbe = Environment.GetEnvironmentVariable("KOMBIFY_TEST_UPDATE_VERIFIER_ROOT");
    var paths = new ClientStatePaths("techstack", "cloud", "beta");
    var state = Path.Combine(paths.DefaultDirectory, $"contract-recovery-{Guid.NewGuid():N}");
    if (paths.ResolveDirectory(state) != state) throw new InvalidOperationException("Unsafe isolated recovery root.");
    var helper = Path.Combine(AppContext.BaseDirectory, DesktopEdition.VerifierFileName);
    if (File.Exists(helper)) throw new InvalidOperationException("The probe cannot replace an existing verifier.");
    var helperCreated = false;
    try
    {
        Environment.SetEnvironmentVariable("HOMELAB_CLIENT_STATE_DIR", state);
        Environment.SetEnvironmentVariable("KOMBIFY_TEST_UPDATE_VERIFIER_ROOT", state);
        Directory.CreateDirectory(state);
        var approved = System.Security.Cryptography.RandomNumberGenerator.GetBytes(64);
        File.WriteAllBytes(Path.Combine(state, "approved-package"), approved);
        // Renaming the compiled test apphost exercises real process selection,
        // cache miss, acquisition and re-verification without an installer or key.
        File.Copy(Path.Combine(AppContext.BaseDirectory, "Kombify.Homelab.Client.ContractTests.exe"), helper);
        helperCreated = true;
        var result = ClientUpdater.ResolveSignedRollbackPackage(new ClientConfig(),
            Path.Combine(ClientUpdater.UpdatesDirectory(), "rollback"), "0.0.0-0");
        if (!File.ReadAllBytes(result.PackagePath).SequenceEqual(approved) || result.PackageSize != approved.Length ||
            !result.PackageSha256.Equals(Convert.ToHexString(System.Security.Cryptography.SHA256.HashData(approved)),
                StringComparison.OrdinalIgnoreCase))
            throw new InvalidDataException("Cloud recovery did not reacquire and reverify its approved package.");
        Console.WriteLine("INFO real Cloud recovery path reacquired a missing cache through the edition helper process; verifier boundary substituted, no production signature claim");
    }
    finally
    {
        Environment.SetEnvironmentVariable("HOMELAB_CLIENT_STATE_DIR", previousState);
        Environment.SetEnvironmentVariable("KOMBIFY_TEST_UPDATE_VERIFIER_ROOT", previousProbe);
        if (helperCreated) File.Delete(helper);
        if (Directory.Exists(state)) Directory.Delete(state, recursive: true);
    }
}

static void AssertCloudUpdateRetainsRecoveryCursor()
{
    if (DesktopEdition.IsLocal) throw new InvalidOperationException("Cloud update proof requires the Cloud build.");
    var previous = Environment.GetEnvironmentVariable("HOMELAB_CLIENT_STATE_DIR");
    var paths = new ClientStatePaths("techstack", "cloud", "beta");
    var state = Path.Combine(paths.DefaultDirectory, $"contract-update-{Guid.NewGuid():N}");
    if (paths.ResolveDirectory(state) != state) throw new InvalidOperationException("Unsafe isolated state root.");
    try
    {
        Environment.SetEnvironmentVariable("HOMELAB_CLIENT_STATE_DIR", state);
        var config = new ClientConfig();
        ClientAuthority.Validate(config);
        try
        {
            ClientAuthority.Validate(config with { UpdateManifestUrl = "https://github.com/kombifyio/Homelab/releases/latest/download/kombify-homelab-windows-update.json" });
            throw new InvalidDataException("Cloud accepted the Local update feed.");
        }
        catch (InvalidOperationException) { }
        var cache = Path.Combine(state, "profiles", "offline-state.json");
        Directory.CreateDirectory(Path.GetDirectoryName(cache)!);
        File.WriteAllText(cache, "pre-update-operator-state");
        var cursor = Path.Combine(ClientUpdater.UpdatesDirectory(), "pending.json");
        Directory.CreateDirectory(Path.GetDirectoryName(cursor)!);
        File.WriteAllText(cursor, "retained-recovery-cursor");
        ClientUpdater.CaptureUpdateSnapshot(config);
        File.WriteAllText(cache, "changed-by-new-client");
        var newCache = Path.Combine(state, "new-client-cache.json");
        File.WriteAllText(newCache, "new-client-state");
        ClientUpdater.RestoreUpdateSnapshot(config);
        if (File.ReadAllText(cache) != "pre-update-operator-state" || File.Exists(newCache) ||
            File.ReadAllText(cursor) != "retained-recovery-cursor")
            throw new InvalidDataException("Cloud rollback lost operator data or its durable recovery cursor.");
        Console.WriteLine("INFO Cloud rollback restores operator cache and retains its recovery cursor; Local feed refused");
    }
    finally
    {
        Environment.SetEnvironmentVariable("HOMELAB_CLIENT_STATE_DIR", previous);
        if (Directory.Exists(state)) Directory.Delete(state, recursive: true);
    }
}

static void AssertFailedUpdatePreservesRecovery()
{
    var previous = Environment.GetEnvironmentVariable("HOMELAB_CLIENT_STATE_DIR");
    var state = Path.Combine(Environment.GetFolderPath(Environment.SpecialFolder.LocalApplicationData),
        "kombify", $"techstack-client-contract-{Guid.NewGuid():N}");
    try
    {
        Environment.SetEnvironmentVariable("HOMELAB_CLIENT_STATE_DIR", state);
        var config = new ClientConfig { AutoUpdate = false };
        var updates = ClientUpdater.UpdatesDirectory();
        var snapshot = Path.Combine(updates, "snapshot");
        var pending = Path.Combine(updates, "pending.json");
        Directory.CreateDirectory(snapshot);
        File.WriteAllText(Path.Combine(snapshot, "operator-data.txt"), "pre-update-data");
        Directory.CreateDirectory(config.RuntimeDataDir);
        var currentData = Path.Combine(config.RuntimeDataDir, "operator-data.txt");
        File.WriteAllText(currentData, "newer-runtime-data");
        File.WriteAllText(pending, System.Text.Json.JsonSerializer.Serialize(new
        {
            from = "0.0.0", to = ClientUpdater.CurrentVersion(), phase = "applying",
            installer = Path.Combine(updates, "unavailable-new-installer.exe"),
            rollbackInstaller = Path.Combine(updates, "unavailable-previous-installer.exe"),
        }));
        var stopped = false;
        ClientUpdater.CompletePendingUpdate(config, false, () => stopped = true);
        Console.WriteLine($"Observed failed-update recovery: cursor retained={File.Exists(pending)}, runtime stopped={stopped}");
        if (!File.Exists(pending) || !stopped)
            throw new InvalidOperationException("A failed update without its recovery installer lost recovery or kept its runtime running.");
        if (ClientUpdater.TryApplyStagedUpdate(config, out var blocked) || string.IsNullOrWhiteSpace(blocked))
            throw new InvalidOperationException("An unrecoverable failed update permitted runtime startup.");
        if (File.ReadAllText(currentData) != "newer-runtime-data" ||
            File.ReadAllText(Path.Combine(snapshot, "operator-data.txt")) != "pre-update-data")
            throw new InvalidOperationException("An unrecoverable failed update changed retained recovery data.");
    }
    finally
    {
        Environment.SetEnvironmentVariable("HOMELAB_CLIENT_STATE_DIR", previous);
        if (Directory.Exists(state)) Directory.Delete(state, recursive: true);
    }
}

static void AssertPendingRollbackResumesBeforeStartup()
{
    var previous = Environment.GetEnvironmentVariable("HOMELAB_CLIENT_STATE_DIR");
    var state = Path.Combine(Environment.GetFolderPath(Environment.SpecialFolder.LocalApplicationData),
        "kombify", $"techstack-client-contract-{Guid.NewGuid():N}");
    var listener = new System.Net.Sockets.TcpListener(System.Net.IPAddress.Loopback, 0);
    using var process = new System.Diagnostics.Process();
    try
    {
        Environment.SetEnvironmentVariable("HOMELAB_CLIENT_STATE_DIR", state);
        listener.Start();
        var port = ((System.Net.IPEndPoint)listener.LocalEndpoint).Port;
        listener.Stop();
        var config = new ClientConfig { AutoUpdate = false, LocalUiUrl = $"http://127.0.0.1:{port}/" };
        var updates = ClientUpdater.UpdatesDirectory();
        var snapshot = Path.Combine(updates, "snapshot");
        var pending = Path.Combine(updates, "pending.json");
        var dataFile = Path.Combine(config.RuntimeDataDir, "operator-data.txt");
        Directory.CreateDirectory(config.RuntimeDataDir);
        Directory.CreateDirectory(snapshot);
        File.WriteAllText(dataFile, "newer-runtime-data");
        File.WriteAllText(Path.Combine(snapshot, "operator-data.txt"), "pre-update-data");

        void SetPending(string from) => File.WriteAllText(pending,
            System.Text.Json.JsonSerializer.Serialize(new { from, to = "0.0.1", phase = "rolling-back" }));
        void AssertDeferred(bool malformed = false)
        {
            var retainedCursor = File.ReadAllBytes(pending);
            if (ClientUpdater.TryApplyStagedUpdate(config, out var reason) || string.IsNullOrWhiteSpace(reason))
                throw new InvalidOperationException("An unfinished rollback permitted runtime startup.");
            ClientUpdater.CompletePendingUpdate(config, true,
                () => throw new InvalidOperationException("Recovery attempted to stop a runtime it does not own."));
            if (!retainedCursor.SequenceEqual(File.ReadAllBytes(pending)))
                throw new InvalidOperationException("A deferred recovery changed its retained cursor.");
            if (malformed) return;
            using var recovery = System.Text.Json.JsonDocument.Parse(File.ReadAllText(pending));
            if (recovery.RootElement.GetProperty("phase").GetString() != "rolling-back")
                throw new InvalidOperationException("Deferred recovery lost its rollback state.");
        }
        void AssertNewDataRetained()
        {
            if (File.ReadAllText(dataFile) != "newer-runtime-data")
                throw new InvalidOperationException("A blocked recovery changed data still owned by the newer runtime.");
        }

        File.WriteAllText(pending, "{\"phase\":\"rolling-back\""); // Interrupted old cursor write.
        AssertDeferred(malformed: true);
        AssertNewDataRetained();
        SetPending("0.0.0"); // The installer has not restored the previous version yet.
        AssertDeferred();
        AssertNewDataRetained();
        SetPending(ClientUpdater.CurrentVersion());
        var retainedSnapshot = Path.Combine(updates, "retained-snapshot");
        Directory.Move(snapshot, retainedSnapshot);
        AssertDeferred();
        AssertNewDataRetained();
        Directory.Move(retainedSnapshot, snapshot);

        // A harmless owned process exercises the production PID/name check.
        // It waits on redirected stdin and never runs database code.
        var probeDirectory = Path.Combine(state, "process-probe");
        Directory.CreateDirectory(probeDirectory);
        var probeExecutable = Path.Combine(probeDirectory, "postgres.exe");
        File.Copy(Environment.GetEnvironmentVariable("ComSpec")!, probeExecutable);
        var start = new System.Diagnostics.ProcessStartInfo(probeExecutable)
        {
            UseShellExecute = false, CreateNoWindow = true,
            RedirectStandardInput = true, RedirectStandardOutput = true, RedirectStandardError = true,
        };
        foreach (var argument in new[] { "/d", "/c", "set /p KOMBIFY_TEST_WAIT=" }) start.ArgumentList.Add(argument);
        process.StartInfo = start;
        if (!process.Start()) throw new InvalidOperationException("The owned observation process did not start.");
        var postgresData = Path.Combine(config.RuntimeDataDir, "postgres", "data");
        Directory.CreateDirectory(postgresData);
        File.WriteAllText(Path.Combine(postgresData, "postmaster.pid"), process.Id.ToString());
        AssertDeferred();
        AssertNewDataRetained();
        process.StandardInput.WriteLine("done");
        process.StandardInput.Close();
        if (!process.WaitForExit(3000)) throw new InvalidOperationException("The owned observation process did not stop.");

        listener = new System.Net.Sockets.TcpListener(System.Net.IPAddress.Loopback, port);
        listener.Start();
        AssertDeferred();
        AssertNewDataRetained();
        listener.Stop();

        // Windows sharing denial interrupts a real copy; the next start must
        // retry using the same retained recovery data, rather than forget it.
        using (var locked = new FileStream(Path.Combine(snapshot, "operator-data.txt"),
                   FileMode.Open, FileAccess.ReadWrite, FileShare.None)) AssertDeferred();
        if (File.ReadAllText(Path.Combine(snapshot, "operator-data.txt")) != "pre-update-data")
            throw new InvalidOperationException("A failed restore changed its recovery data.");
        if (ClientUpdater.TryApplyStagedUpdate(config, out var blocked) || blocked is not null ||
            File.ReadAllText(dataFile) != "pre-update-data")
            throw new InvalidOperationException("Recovery did not restore the pre-update data before allowing startup.");
        File.WriteAllText(dataFile, "data-written-after-recovery");
        if (ClientUpdater.TryApplyStagedUpdate(config, out blocked) || blocked is not null ||
            File.ReadAllText(dataFile) != "data-written-after-recovery")
            throw new InvalidOperationException("A committed restore blocked startup or replayed over newer data.");
    }
    finally
    {
        listener.Stop();
        try { if (!process.HasExited) { process.Kill(entireProcessTree: true); process.WaitForExit(3000); } }
        catch (InvalidOperationException) { }
        Environment.SetEnvironmentVariable("HOMELAB_CLIENT_STATE_DIR", previous);
        if (Directory.Exists(state)) Directory.Delete(state, recursive: true);
    }
}

#if CLOUD_DESKTOP
static async Task AssertConnectAccountMismatchDenied()
{
    var account = new ConnectProductIdentity("account-A", "tenant-A");
    var credentialWrites = 0;
    var connectRequests = 0;
    try
    {
        await ConnectEnrollment.WithVerifiedSessionAsync(account,
            _ => Task.FromResult(account),
            _ => Task.FromResult(new Auth0NativeSession("token-B", "refresh-B", "account-B")),
            _ => credentialWrites++,
            (_, _) =>
            {
                connectRequests++;
                return Task.FromResult(true);
            }, CancellationToken.None);
    }
    catch (ConnectAccountException)
    {
        if (credentialWrites == 0 && connectRequests == 0) return;
        throw new InvalidOperationException("Mismatched Connect login changed account custody or called Connect.");
    }
    throw new InvalidOperationException("Mismatched Connect login was accepted.");
}

// Rule A2 (signed in once per device), three classes: transient (429, 5xx, network faults, timeouts, answers without an
// OAuth error body, authorization_pending / slow_down / too_many_requests) keeps the stored refresh token and backs off;
// an operator fault (invalid_client, unauthorized_client) keeps it and reports "not ready"; every other OAuth error on
// 400/401/403 is the session gone.
static async Task AssertConnectRefreshFailureRule()
{
    static string Jwt() => "e30." + Convert.ToBase64String("{\"sub\":\"auth0|connect-refresh\"}"u8.ToArray())
        .TrimEnd('=').Replace('+', '-').Replace('/', '_') + ".c2ln";
    static ScriptedAuth0 Script(params object[] answers) => new(answers);
    static (Auth0NativeLogin Client, List<TimeSpan> Waits, Func<TimeSpan, CancellationToken, Task> Delay) Make(ScriptedAuth0 auth0)
    {
        var waits = new List<TimeSpan>();
        return (new Auth0NativeLogin(new HttpClient(auth0), new Uri("https://login.kombify.test/"), "client", "https://api.kombify.test", [63690]),
            waits, (wait, _) => { waits.Add(wait); return Task.CompletedTask; });
    }
    static ScriptedAuth0.Answer Ok(string? rotated = null) => new(System.Net.HttpStatusCode.OK,
        "{\"access_token\":\"" + Jwt() + "\"" + (rotated is null ? "" : ",\"refresh_token\":\"" + rotated + "\"") + "}");
    static ScriptedAuth0.Answer Err(int status, string body, int? retryAfter = null) => new((System.Net.HttpStatusCode)status, body, retryAfter);
    static void Expect(bool condition, string what) { if (!condition) throw new InvalidOperationException("Connect refresh rule: " + what); }

    // Transient: a 5xx, then a network fault, then a timeout (retried at once with the same token), then success.
    var recovering = Script(Err(503, "{}"), new HttpRequestException("network"), new TaskCanceledException("timeout"), Ok("refresh-rotated"));
    var (login, waits, delay) = Make(recovering);
    var session = await login.RefreshWithBackoffAsync("refresh-old", CancellationToken.None, delay);
    Expect(session.RefreshToken == "refresh-rotated" && session.SubjectId == "auth0|connect-refresh", "rotated token and subject not returned");
    Expect(recovering.RefreshTokens.SequenceEqual(["refresh-old", "refresh-old", "refresh-old", "refresh-old"]), "the stored token was not replayed unchanged");
    Expect(waits.SequenceEqual([TimeSpan.FromSeconds(1), TimeSpan.FromSeconds(2)]), "a timeout was not retried at once (expected waits 1, 2 s only)");

    // A timeout allows at most one immediate retry; if that fails (timeout or 5xx), nothing more is sent and not ready is reported.
    var twice = Script(new TaskCanceledException("timeout"), new TaskCanceledException("timeout"), Ok("refresh-rotated"));
    (login, waits, delay) = Make(twice);
    try { await login.RefreshWithBackoffAsync("refresh-old", CancellationToken.None, delay); Expect(false, "a double timeout refreshed"); }
    catch (Auth0RefreshUnavailableException ex) when (ex is not Auth0ClientNotReadyException)
    {
        Expect(twice.RefreshTokens.Count == 2 && waits.Count == 0 && ex.PossiblyRetired && ex.Message.Contains("not ready", StringComparison.Ordinal)
            && !ex.Message.Contains("try again in a moment", StringComparison.Ordinal),
            "a double timeout replayed past the immediate retry or misreported not ready");
    }
    var timeoutThen503 = Script(new TaskCanceledException("timeout"), Err(503, "{}"), Ok("refresh-rotated"));
    (login, waits, delay) = Make(timeoutThen503);
    try { await login.RefreshWithBackoffAsync("refresh-old", CancellationToken.None, delay); Expect(false, "timeout then 503 refreshed"); }
    catch (Auth0RefreshUnavailableException ex) when (ex is not Auth0ClientNotReadyException)
    {
        Expect(timeoutThen503.RefreshTokens.Count == 2 && waits.Count == 0 && ex.PossiblyRetired,
            "a 503 after a timed-out send entered backoff and replayed a possibly-retired token");
    }

    // 429 waits its Retry-After.
    var limited = Script(Err(429, "{\"error\":\"too_many_requests\"}", 3), Ok());
    (login, waits, delay) = Make(limited);
    await login.RefreshWithBackoffAsync("refresh-old", CancellationToken.None, delay);
    Expect(waits.SequenceEqual([TimeSpan.FromSeconds(3)]), "Retry-After not honoured");

    // A persistent 5xx is temporary, never a refused grant (even with invalid_grant in its body). The last allowed
    // attempt ends the loop: 4 sends and 3 waits, never a further send without a wait.
    var down = Script(Err(502, "{\"error\":\"invalid_grant\"}"));
    (login, waits, delay) = Make(down);
    try { await login.RefreshWithBackoffAsync("refresh-old", CancellationToken.None, delay); Expect(false, "a 5xx refreshed"); }
    catch (Auth0RefreshUnavailableException ex) when (ex is not Auth0ClientNotReadyException)
    {
        Expect(down.RefreshTokens.Count == 4 && waits.Count == 3 && !ex.PossiblyRetired,
            "a persistent 5xx was not retried exactly 4 times with 3 waits, or was called possibly retired");
    }

    // A connection lost after the request was written may have retired the token: the sends stay inside the budget,
    // and the failure carries the marker that stops later sends in this run.
    var reset = Script(new HttpRequestException("The response ended prematurely."));
    (login, waits, delay) = Make(reset);
    try { await login.RefreshWithBackoffAsync("refresh-old", CancellationToken.None, delay); Expect(false, "a reset refreshed"); }
    catch (Auth0RefreshUnavailableException ex)
    {
        Expect(reset.RefreshTokens.Count == 4 && waits.Count == 3 && ex.PossiblyRetired
            && ex.Message.Contains("not ready", StringComparison.Ordinal),
            "a connection reset did not keep the possibly-retired marker");
    }

    // A connection that never reached Auth0 (name resolution, connect) retires nothing: the token stays replayable.
    var unreachable = Script(new HttpRequestException(System.Net.Http.HttpRequestError.NameResolutionError,
        "No such host is known."));
    (login, waits, delay) = Make(unreachable);
    try { await login.RefreshWithBackoffAsync("refresh-old", CancellationToken.None, delay); Expect(false, "an unreachable host refreshed"); }
    catch (Auth0RefreshUnavailableException ex)
    {
        Expect(unreachable.RefreshTokens.Count == 4 && !ex.PossiblyRetired,
            "a name-resolution fault was called possibly retired");
    }

    // A 200 whose body carries no usable token may have rotated and retired the sent token: one send, no retry,
    // and the marker that stops later sends in this run.
    var unusable = Script(new ScriptedAuth0.Answer(System.Net.HttpStatusCode.OK, "<html>proxy</html>"));
    (login, waits, delay) = Make(unusable);
    try { await login.RefreshWithBackoffAsync("refresh-old", CancellationToken.None, delay); Expect(false, "an unusable 200 refreshed"); }
    catch (Auth0RefreshUnavailableException ex)
    {
        Expect(unusable.RefreshTokens.Count == 1 && waits.Count == 0 && ex.PossiblyRetired
            && ex.Message.Contains("not ready", StringComparison.Ordinal)
            && !ex.Message.Contains("try again in a moment", StringComparison.Ordinal),
            "an unusable 200 body was retried or did not report a possibly-retired token");
    }

    // A send the caller cancelled in flight may have been received: the exception stays a cancellation and keeps the
    // marker, so nothing more is sent in this run.
    using (var cancel = new CancellationTokenSource())
    {
        var inFlightAnswer = new ScriptedAuth0.InFlight();
        var inFlight = Script(inFlightAnswer);
        (login, waits, delay) = Make(inFlight);
        var call = login.RefreshWithBackoffAsync("refresh-old", cancel.Token, delay);
        await inFlightAnswer.Sent.Task;
        cancel.Cancel();
        try { await call; Expect(false, "a cancelled send refreshed"); }
        catch (OperationCanceledException ex)
        {
            Expect(inFlight.RefreshTokens.Count == 1 && waits.Count == 0
                && ex is ConnectRefreshCancelledException { PossiblyRetired: true },
                "a cancelled in-flight send did not keep the possibly-retired marker");
        }
    }

    // A Retry-After beyond the budget is not waited out here, is capped at one hour, and omits "try again in a moment".
    var tooLong = Script(Err(429, "{}", 86400));
    (login, waits, delay) = Make(tooLong);
    try { await login.RefreshWithBackoffAsync("refresh-old", CancellationToken.None, delay); Expect(false, "a 429 refreshed"); }
    catch (Auth0RefreshUnavailableException ex)
    {
        Expect(tooLong.RefreshTokens.Count == 1 && waits.Count == 0 && ex.RetryAfter == TimeSpan.FromHours(1)
            && ex.Message.Contains("1 hour", StringComparison.Ordinal)
            && !ex.Message.Contains("try again in a moment", StringComparison.Ordinal),
            "an over-budget Retry-After was waited out, not capped at an hour, or kept generic wording");
    }

    // Transient too: no OAuth error body (an HTML 403, a bodiless 400, a proxy 401) and the transient codes.
    foreach (var (status, body) in new[] { (403, "<html>blocked</html>"), (400, ""), (401, "{\"message\":\"unauthorized\"}"),
        (403, "{\"error\":\"too_many_requests\"}"), (400, "{\"error\":\"slow_down\"}"), (400, "{\"error\":\"authorization_pending\"}") })
    {
        var quiet = Script(Err(status, body));
        (login, waits, delay) = Make(quiet);
        try { await login.RefreshWithBackoffAsync("refresh-old", CancellationToken.None, delay); Expect(false, $"{status} {body} refreshed"); }
        catch (Auth0RefreshUnavailableException ex) when (ex is not Auth0ClientNotReadyException)
        { Expect(quiet.RefreshTokens.Count == 4 && waits.Count == 3, $"{status} {body} was not treated as transient"); }
    }

    // Operator fault: the credential is kept, the program is not ready, no retry in this process, no "try again in a moment".
    foreach (var (status, error) in new[] { (401, "invalid_client"), (403, "unauthorized_client"), (400, "invalid_client") })
    {
        var operatorFault = Script(Err(status, "{\"error\":\"" + error + "\"}"));
        (login, waits, delay) = Make(operatorFault);
        try { await login.RefreshWithBackoffAsync("refresh-old", CancellationToken.None, delay); Expect(false, error + " refreshed"); }
        catch (Auth0ClientNotReadyException ex)
        {
            Expect(operatorFault.RefreshTokens.Count == 1 && waits.Count == 0
                && !ex.Message.Contains("try again in a moment", StringComparison.Ordinal),
                error + " was retried or kept 'try again in a moment'");
        }
    }

    // Definitive: every other OAuth error on 400/401/403 ends the session at once, with no retry.
    foreach (var (status, error) in new[] { (403, "invalid_grant"), (403, "access_denied"), (400, "expired_token"), (403, "mfa_required"),
        (400, "invalid_request"), (401, "a_code_nobody_knows") })
    {
        var refused = Script(Err(status, "{\"error\":\"" + error + "\"}"));
        (login, waits, delay) = Make(refused);
        try { await login.RefreshWithBackoffAsync("refresh-old", CancellationToken.None, delay); Expect(false, error + " refreshed"); }
        catch (Auth0GrantRefusedException) { Expect(refused.RefreshTokens.Count == 1 && waits.Count == 0, error + " was retried"); }
    }

    // Fix 2: a rotated token for the expected account is persisted before post-sign-in account checks run.
    var switchedWrites = 0;
    var switchedConnectRequests = 0;
    var expectedIdentity = new ConnectProductIdentity("account-A", "tenant-A");
    var identityReads = 0;
    try
    {
        await ConnectEnrollment.WithVerifiedSessionAsync(expectedIdentity,
            _ => Task.FromResult(identityReads++ == 0 ? expectedIdentity : new ConnectProductIdentity("account-B", "tenant-B")),
            _ => Task.FromResult(new Auth0NativeSession("token-A2", "refresh-A2", "account-A")),
            _ => switchedWrites++,
            (_, _) => { switchedConnectRequests++; return Task.FromResult(true); },
            CancellationToken.None);
        Expect(false, "an account switch after refresh reached Connect");
    }
    catch (ConnectAccountException)
    {
        Expect(switchedWrites == 1 && switchedConnectRequests == 0, "the rotated token was not saved before the post-sign-in account check");
    }

    // The enrolment keeps the Credential Manager entry on a temporary failure and does not fall into the browser;
    // once a timed-out send may have retired the token, later calls in this run send nothing more and report not ready.
    var previous = Environment.GetEnvironmentVariable("HOMELAB_CLIENT_STATE_DIR");
    var paths = new ClientStatePaths("techstack", "cloud", "beta");
    var state = Path.Combine(paths.DefaultDirectory, $"contract-refresh-rule-{Guid.NewGuid():N}");
    if (paths.ResolveDirectory(state) != state) throw new InvalidOperationException("Unsafe isolated refresh-rule root.");
    const string subject = "auth0|connect-refresh";
    try
    {
        Environment.SetEnvironmentVariable("HOMELAB_CLIENT_STATE_DIR", state);
        var target = ConnectEnrollment.RefreshTarget(subject);
        WindowsCredentialStore.Write(target, subject, "refresh-old");
        using var http = new HttpClient(Script(new ScriptedAuth0.Answer(System.Net.HttpStatusCode.ServiceUnavailable, "{}")));
        try
        {
            using var limit = new CancellationTokenSource(TimeSpan.FromSeconds(30));
            await ConnectEnrollment.SignInForAccountAsync(new ClientConfig(), http, subject, limit.Token);
            Expect(false, "the enrolment signed in while Auth0 was down");
        }
        catch (Auth0RefreshUnavailableException) { }
        Expect(WindowsCredentialStore.Read(target) == "refresh-old", "the stored refresh token did not survive a 5xx");
        using var unregistered = new HttpClient(Script(Err(401, "{\"error\":\"invalid_client\"}")));
        try
        {
            using var limit = new CancellationTokenSource(TimeSpan.FromSeconds(30));
            await ConnectEnrollment.SignInForAccountAsync(new ClientConfig(), unregistered, subject, limit.Token);
            Expect(false, "the enrolment signed in while the client was not registered");
        }
        catch (Auth0ClientNotReadyException) { }
        Expect(WindowsCredentialStore.Read(target) == "refresh-old", "the stored refresh token did not survive invalid_client");

        // An account-switching SignInForAccountAsync call still persists the rotated token before WithVerifiedSessionAsync checks.
        using var rotatedHttp = new HttpClient(Script(Ok("refresh-rotated-early")));
        var earlySwitchedConnect = 0;
        var earlyIdentityReads = 0;
        var enrolledIdentity = new ConnectProductIdentity(subject, "tenant-A");
        try
        {
            using var limit = new CancellationTokenSource(TimeSpan.FromSeconds(30));
            await ConnectEnrollment.WithVerifiedSessionAsync(enrolledIdentity,
                _ => Task.FromResult(earlyIdentityReads++ == 0 ? enrolledIdentity : new ConnectProductIdentity("other-subject", "tenant-B")),
                ct => ConnectEnrollment.SignInForAccountAsync(new ClientConfig(), rotatedHttp, subject, ct),
                session => ConnectEnrollment.SaveRefreshForAccount(subject, session),
                (_, _) => { earlySwitchedConnect++; return Task.FromResult(true); },
                limit.Token);
            Expect(false, "the enrolment continued after the account changed");
        }
        catch (ConnectAccountException)
        {
            Expect(earlySwitchedConnect == 0 && WindowsCredentialStore.Read(target) == "refresh-rotated-early",
                "SignInForAccountAsync did not save the rotated token before the account check failed");
        }

        // A timed-out send marks the token as possibly retired for this run: only 2 attempts, then 0 on repeat in this run.
        var timedOutScript = Script(new TaskCanceledException("timeout"), new TaskCanceledException("timeout"), Ok("refresh-after-timeout"));
        using var timedOutHttp = new HttpClient(timedOutScript);
        try
        {
            using var limit = new CancellationTokenSource(TimeSpan.FromSeconds(30));
            await ConnectEnrollment.SignInForAccountAsync(new ClientConfig(), timedOutHttp, subject, limit.Token);
            Expect(false, "the enrolment signed in after double timeout");
        }
        catch (Auth0RefreshUnavailableException ex) when (ex.PossiblyRetired) { }
        Expect(timedOutScript.RefreshTokens.Count == 2 && WindowsCredentialStore.Read(target) == "refresh-rotated-early",
            "timed-out enrolment sent more than 2 requests or dropped the credential");

        var secondRunScript = Script(Ok("should-never-send"));
        using var secondRunHttp = new HttpClient(secondRunScript);
        try
        {
            using var limit = new CancellationTokenSource(TimeSpan.FromSeconds(30));
            await ConnectEnrollment.SignInForAccountAsync(new ClientConfig(), secondRunHttp, subject, limit.Token);
            Expect(false, "a repeated enrolment call in the same run sent a possibly-retired token");
        }
        catch (Auth0RefreshUnavailableException ex) when (ex.PossiblyRetired) { }
        Expect(secondRunScript.RefreshTokens.Count == 0, "a repeated enrolment call in the same run contacted Auth0");

        // A send the caller cancelled in flight marks the token too: the exception stays a cancellation, and the next
        // call in this run sends nothing.
        const string cancelledSubject = "auth0|connect-cancelled";
        var cancelledTarget = ConnectEnrollment.RefreshTarget(cancelledSubject);
        WindowsCredentialStore.Write(cancelledTarget, cancelledSubject, "refresh-cancelled");
        var cancelledAnswer = new ScriptedAuth0.InFlight();
        var cancelledInFlight = Script(cancelledAnswer);
        using var cancelledHttp = new HttpClient(cancelledInFlight);
        using var cancelSource = new CancellationTokenSource();
        var cancelledCall = ConnectEnrollment.SignInForAccountAsync(new ClientConfig(), cancelledHttp, cancelledSubject, cancelSource.Token);
        await cancelledAnswer.Sent.Task;
        cancelSource.Cancel();
        try
        {
            await cancelledCall;
            Expect(false, "a cancelled enrolment completed");
        }
        catch (OperationCanceledException) { }
        var afterCancelScript = Script(Ok("should-never-send"));
        using var afterCancelHttp = new HttpClient(afterCancelScript);
        try
        {
            using var limit = new CancellationTokenSource(TimeSpan.FromSeconds(30));
            await ConnectEnrollment.SignInForAccountAsync(new ClientConfig(), afterCancelHttp, cancelledSubject, limit.Token);
            Expect(false, "a cancelled send did not stop the next send in this run");
        }
        catch (Auth0RefreshUnavailableException ex) when (ex.PossiblyRetired) { }
        Expect(afterCancelScript.RefreshTokens.Count == 0, "a cancelled send left the token replayable in this run");
    }
    finally
    {
        // Cleanup never replaces the result above: a logon without a credential vault answers 1312 on delete.
        TryDeleteCredential(ConnectEnrollment.RefreshTarget(subject));
        TryDeleteCredential(ConnectEnrollment.RefreshTarget("auth0|connect-cancelled"));
        Environment.SetEnvironmentVariable("HOMELAB_CLIENT_STATE_DIR", previous);
    }
}
#endif

static void AssertLocalStartupEntry()
{
    var previous = Environment.GetEnvironmentVariable("HOMELAB_CLIENT_STATE_DIR");
    var state = Path.Combine(
        Environment.GetFolderPath(Environment.SpecialFolder.LocalApplicationData),
        "kombify", $"techstack-client-startup-{Guid.NewGuid():N}");
    try
    {
        Environment.SetEnvironmentVariable("HOMELAB_CLIENT_STATE_DIR", state);
        AssertEntry(ClientConfig.Load([]), "http://127.0.0.1:5260/client/local?client=windows");

        Directory.CreateDirectory(state);
        foreach (var path in new[] { "onboarding", "local" })
        {
            File.WriteAllText(ClientConfig.ConfigPath(),
                $$"""{"mode":"local","localOnboardingUrl":"http://127.0.0.1:5260/client/{{path}}?client=windows"}""");
            AssertEntry(ClientConfig.Load([]), "http://127.0.0.1:5260/client/local?client=windows");
        }

        MustRejectLocalCloud(["--cloud-ui-url", "https://cloud.example.test/"]);
        MustRejectLocalCloud(["--mode", "cloud"]);
        MustRejectLocalCloud(["--connect-enroll"]);
        AssertEntry(ClientConfig.Load(["--url", "https://home.example.test/"]),
            "https://home.example.test/");
        AssertEntry(ClientConfig.Load(["--local-ui-url", "http://127.0.0.1:6270/", "--local-onboarding-url", "http://127.0.0.1:6270/client/local?client=windows"]),
            "http://127.0.0.1:6270/client/local?client=windows");
    }
    finally
    {
        Environment.SetEnvironmentVariable("HOMELAB_CLIENT_STATE_DIR", previous);
        if (File.Exists(Path.Combine(state, "client.json"))) File.Delete(Path.Combine(state, "client.json"));
        if (Directory.Exists(state)) Directory.Delete(state);
    }
}

static void AssertEntry(ClientConfig config, string expected)
{
    if (!string.Equals(config.InitialUrl(), expected, StringComparison.Ordinal))
    {
        throw new InvalidOperationException($"Client opened {config.InitialUrl()} instead of {expected}.");
    }
}

static void AssertDeviceContextDocumentBoundary()
{
    var selected = DesktopEdition.IsLocal ? "http://127.0.0.1:5260/" : ClientAuthority.CloudOrigin;
    var document = selected + "settings";
    var replies = 0;
    void Deliver(string source, string current, long revision, bool navigating, string token = "current-document")
    {
        if (ClientAuthority.AllowsDeviceContextReply(source, current, selected, 4, revision, token, "current-document", navigating))
            replies++;
    }
    Deliver("https://other.example/settings", document, 4, false);
    Deliver(document, "https://other.example/settings", 4, false);
    Deliver(selected + "dashboard", document, 4, false);
    Deliver(document, document, 5, false); // Same URL after a new navigation.
    Deliver(document, document, 4, true);
    Deliver(document, document, 4, false, "previous-document");
    if (replies != 0) throw new InvalidOperationException("A foreign or replaced document received device context.");
    Deliver(document, document, 4, false);
    if (replies != 1) throw new InvalidOperationException("The current product document could not read its context.");
}

static void AssertInstallerAuthority()
{
    var previous = Environment.GetEnvironmentVariable("HOMELAB_CLIENT_STATE_DIR");
    var root = Path.Combine(Environment.GetFolderPath(Environment.SpecialFolder.LocalApplicationData), "kombify");
    var state = DesktopEdition.IsLocal
        ? Path.Combine(root, $"techstack-client-contract-{Guid.NewGuid():N}")
        : Path.Combine(root, "techstack", "cloud", "beta", $"contract-{Guid.NewGuid():N}");
    try
    {
        Environment.SetEnvironmentVariable("HOMELAB_CLIENT_STATE_DIR", state);
        Directory.CreateDirectory(state);
        var rejected = DesktopEdition.IsLocal
            ? new[] {
                """{"localUiUrl":"https://remote.example/"}""",
                """{"localOnboardingUrl":"http://127.0.0.1:6270/"}""",
                """{"localUiUrl":"http://user@127.0.0.1:5260/"}"""
            }
            : new[] {
                """{"cloudUiUrl":"http://127.0.0.1:5260/"}""",
                """{"cloudUiUrl":"https://homelab.kombify.io.evil.example/"}""",
                """{"connectAuth0Issuer":"https://other.example/"}""",
                """{"cloudDevicePollEndpoint":"https://other.example/poll"}""",
                """{"cloudDevicePollEndpoint":"https://app.kombify.io/other"}"""
            };
        foreach (var json in rejected)
        {
            File.WriteAllText(ClientConfig.ConfigPath(), json);
            try { ClientConfig.Load([]); }
            catch (InvalidOperationException) { continue; }
            throw new InvalidOperationException("A persisted endpoint escaped the installer authority.");
        }
        File.Delete(ClientConfig.ConfigPath());
        var config = ClientConfig.Load([]);
        if (!ClientAuthority.AllowsProductNavigation(new Uri(config.InitialUrl()), config.InitialUrl())
            || ClientAuthority.AllowsProductNavigation(new Uri("https://other.example/"), config.InitialUrl())
            || (!DesktopEdition.IsLocal && ClientAuthority.AllowsProductNavigation(new Uri("http://127.0.0.1:5260/"), config.InitialUrl())))
            throw new InvalidOperationException("Navigation did not preserve the installed product authority.");
        var arguments = DesktopEdition.IsLocal
            ? new[] { "--local-ui-url", "https://other.example/" }
            : new[] { "--cloud-ui-url", "http://127.0.0.1:5260/" };
        try { ClientConfig.Load(arguments); }
        catch (InvalidOperationException) { return; }
        throw new InvalidOperationException("A command-line endpoint escaped the installer authority.");
    }
    finally
    {
        Environment.SetEnvironmentVariable("HOMELAB_CLIENT_STATE_DIR", previous);
        if (File.Exists(Path.Combine(state, "client.json"))) File.Delete(Path.Combine(state, "client.json"));
        if (Directory.Exists(state)) Directory.Delete(state);
    }
}

static async Task AssertCredentialRedirectRejected()
{
    var reservation = new System.Net.Sockets.TcpListener(System.Net.IPAddress.Loopback, 0);
    reservation.Start();
    var port = ((System.Net.IPEndPoint)reservation.LocalEndpoint).Port;
    reservation.Stop();
    using var listener = new System.Net.HttpListener();
    var origin = $"http://127.0.0.1:{port}/";
    listener.Prefixes.Add(origin);
    listener.Start();
    using var http = ClientAuthority.CreateHttpClient();
    using var timeout = new CancellationTokenSource(TimeSpan.FromSeconds(5));
    using var request = new HttpRequestMessage(HttpMethod.Post, origin + "bootstrap");
    request.Headers.Add("X-Homelab-Device-Token", "isolated-test-token");
    var responseTask = http.SendAsync(request, timeout.Token);
    var context = await listener.GetContextAsync().WaitAsync(timeout.Token);
    context.Response.StatusCode = 307;
    context.Response.RedirectLocation = origin + "credential-sink";
    context.Response.Close();
    using var response = await responseTask;
    if (response.StatusCode != System.Net.HttpStatusCode.TemporaryRedirect)
        throw new InvalidOperationException("The credential-bearing request followed a redirect.");
}

static async Task AssertSpoofedLocalRuntimeRejected()
{
    var reservation = new System.Net.Sockets.TcpListener(System.Net.IPAddress.Loopback, 0);
    reservation.Start();
    var port = ((System.Net.IPEndPoint)reservation.LocalEndpoint).Port;
    reservation.Stop();
    using var listener = new System.Net.HttpListener();
    var origin = $"http://127.0.0.1:{port}/";
    listener.Prefixes.Add(origin);
    listener.Start();
    using var http = ClientAuthority.CreateHttpClient();
    using var timeout = new CancellationTokenSource(TimeSpan.FromSeconds(5));
    var secret = new string('a', 64);
    var authentication = LocalDeviceSessionProof.AuthenticateAsync(http, new Uri(origin), secret, timeout.Token);
    var context = await listener.GetContextAsync().WaitAsync(timeout.Token);
    if (context.Request.Url?.AbsolutePath != LocalDeviceSessionProof.ChallengePath
        || context.Request.Headers["X-Homelab-Device-Token"] is not null
        || context.Request.Headers["X-Homelab-Device-Proof"] is not null)
        throw new InvalidOperationException("A spoof listener received the raw credential or a session proof before authentication.");
    context.Response.ContentType = "application/json";
    var fake = System.Text.Encoding.UTF8.GetBytes(
        "{\"data\":{\"server_nonce\":\"" + new string('b', 32) + "\",\"proof\":\"" + new string('c', 64) + "\"}}");
    context.Response.OutputStream.Write(fake);
    context.Response.Close();
    try { await authentication; }
    catch (InvalidOperationException) { return; }
    throw new InvalidOperationException("A spoof listener passed local runtime identity verification.");
}

static void MustRejectLocalCloud(string[] arguments)
{
    try { ClientConfig.Load(arguments); }
    catch (InvalidOperationException) { return; }
    throw new InvalidOperationException("The Standalone client accepted a Cloud or Connect entry point.");
}

static void AssertLocalEditionRejectsCloudProfile()
{
    var cloud = ValidSelfHosted.Replace("\"self_hosted\"", "\"cloud\"")
        .Replace("\"device_authorization\"", "\"authorization_code_pkce\"");
    var profile = Parse(cloud, "https://home.example.net/");
    MustFail(() => ClientConfig.ValidateProfileAuthority(profile));
}

static ClientConnectionProfile Parse(string json, string endpoint)
{
    return ClientConnectionProfileValidator.ParseAndValidate(
        json,
        ClientConnectionProfileValidator.NormalizeConfiguredEndpoint(endpoint));
}

static void MustFail(Action action)
{
    try
    {
        action();
    }
    catch (ClientProfileException)
    {
        return;
    }
    throw new InvalidOperationException("Expected the profile to fail closed.");
}

static void AssertDefaultCredentialTarget()
{
    const string baseTarget = "kombify/techstack/local/device-session-token";
    var previous = Environment.GetEnvironmentVariable("HOMELAB_CLIENT_STATE_DIR");
    try
    {
        Environment.SetEnvironmentVariable("HOMELAB_CLIENT_STATE_DIR", null);
        if (!string.Equals(ClientConfig.CredentialTarget(baseTarget), baseTarget, StringComparison.Ordinal))
        {
            throw new InvalidOperationException("Default product credential target changed unexpectedly.");
        }
    }
    finally
    {
        Environment.SetEnvironmentVariable("HOMELAB_CLIENT_STATE_DIR", previous);
    }
}

static void AssertIsolatedCredentialNamespace()
{
    const string baseTarget = "kombify/techstack/local/device-session-token";
    var previous = Environment.GetEnvironmentVariable("HOMELAB_CLIENT_STATE_DIR");
    try
    {
        var root = Path.Combine(
            Environment.GetFolderPath(Environment.SpecialFolder.LocalApplicationData),
            "kombify");
        var firstState = Path.Combine(root, "techstack-client-contract-a");
        var secondState = Path.Combine(root, "techstack-client-contract-b");

        Environment.SetEnvironmentVariable("HOMELAB_CLIENT_STATE_DIR", firstState);
        var first = ClientConfig.CredentialTarget(baseTarget);
        var replay = ClientConfig.CredentialTarget(baseTarget);
        Environment.SetEnvironmentVariable("HOMELAB_CLIENT_STATE_DIR", secondState);
        var second = ClientConfig.CredentialTarget(baseTarget);

        if (!first.StartsWith($"{baseTarget}/state-", StringComparison.Ordinal)
            || first.Length != baseTarget.Length + "/state-".Length + 16
            || !string.Equals(first, replay, StringComparison.Ordinal)
            || string.Equals(first, second, StringComparison.Ordinal))
        {
            throw new InvalidOperationException("State-scoped credential target is missing, unstable, or colliding.");
        }
    }
    finally
    {
        Environment.SetEnvironmentVariable("HOMELAB_CLIENT_STATE_DIR", previous);
    }
}

static void AssertRejectedStateOverride(string rootSuffix)
{
    var previous = Environment.GetEnvironmentVariable("HOMELAB_CLIENT_STATE_DIR");
    try
    {
        var localAppData = Environment.GetFolderPath(Environment.SpecialFolder.LocalApplicationData);
        var rejected = Path.Combine(localAppData, $"kombify{rootSuffix}");
        var expected = Path.Combine(localAppData, "kombify", "techstack-client");
        Environment.SetEnvironmentVariable("HOMELAB_CLIENT_STATE_DIR", rejected);
        if (!string.Equals(ClientConfig.StateDirectory(), expected, StringComparison.OrdinalIgnoreCase))
        {
            throw new InvalidOperationException($"Unsafe state override was accepted: {rejected}");
        }
    }
    finally
    {
        Environment.SetEnvironmentVariable("HOMELAB_CLIENT_STATE_DIR", previous);
    }
}

// Opt-in focused production API/store consumer. No WebView, provider or hosted
// authority is involved; the runtime, state and credential targets are owned.
static async Task WindowsLocalWalletDeviceRevealBoundary(string[] arguments)
{
    string RequiredPath(string name) => Path.GetFullPath(arguments.Single(value => value.StartsWith(name + "=", StringComparison.Ordinal))[(name.Length + 1)..]);
    var executable = RequiredPath("--runtime-exe");
    var bundle = RequiredPath("--postgres-bundle");
    var expectedRevision = arguments.FirstOrDefault(value => value.StartsWith("--source-sha=", StringComparison.Ordinal))?[13..];
    if (!File.Exists(executable) || !Directory.Exists(bundle)) throw new InvalidOperationException("Wallet probe runtime/bundle unavailable.");
    var previousState = Environment.GetEnvironmentVariable("HOMELAB_CLIENT_STATE_DIR");
    Environment.SetEnvironmentVariable("HOMELAB_CLIENT_STATE_DIR", null);
    var root = ClientConfig.StateDirectory();
    var state = Path.Combine(root, "wallet-live-probe-" + Guid.NewGuid().ToString("N"));
    Environment.SetEnvironmentVariable("HOMELAB_CLIENT_STATE_DIR", state);
    var credentialTargets = new List<string>();
    using var runtime = new SupervisedProcess();
    var dataDirectory = Path.Combine(state, "runtime");
    var origin = new Uri("http://127.0.0.1:" + WalletProbePort() + "/");
    var postgresPort = WalletProbePort();
    try
    {
        if (ClientConfig.StateDirectory() != state) throw new InvalidOperationException("Wallet probe state is not isolated.");
        Directory.CreateDirectory(state);
        File.WriteAllText(Path.Combine(state, "homelab.yaml"), "{}");
        var ownBundle = Path.Combine(state, "postgres-bundle");
        Directory.CreateDirectory(ownBundle);
        using var manifest = System.Text.Json.JsonDocument.Parse(File.ReadAllText(Path.Combine(bundle, "bundle.json")));
        var archive = manifest.RootElement.GetProperty("archive").GetString()!;
        if (Path.GetFileName(archive) != archive) throw new InvalidOperationException("Unsafe bundle archive.");
        foreach (var file in new[] { "bundle.json", "COPYRIGHT", archive }) File.Copy(Path.Combine(bundle, file), Path.Combine(ownBundle, file));
        using (var stream = File.OpenRead(Path.Combine(ownBundle, archive)))
            if (Convert.ToHexString(System.Security.Cryptography.SHA256.HashData(stream)).ToLowerInvariant()
                != manifest.RootElement.GetProperty("archive_sha256").GetString()) throw new InvalidOperationException("Copied PostgreSQL bundle integrity failed.");
        string Credential(string name, string value)
        {
            var target = ClientConfig.CredentialTarget("kombify/techstack/local/" + name);
            credentialTargets.Add(target);
            WindowsCredentialStore.Write(target, Environment.UserName, value);
            return WindowsCredentialStore.Read(target) ?? throw new InvalidOperationException("Wallet probe custody unavailable.");
        }
        var device = Credential("device-session-token", Convert.ToHexString(System.Security.Cryptography.RandomNumberGenerator.GetBytes(32)).ToLowerInvariant());
        var signing = Credential("runtime-session-secret", Convert.ToHexString(System.Security.Cryptography.RandomNumberGenerator.GetBytes(32)).ToLowerInvariant());
        var encryption = Credential("runtime-encryption-key", Convert.ToBase64String(System.Security.Cryptography.RandomNumberGenerator.GetBytes(24)).Replace('+', '-').Replace('/', '_'));
        async Task Start()
        {
            var start = new System.Diagnostics.ProcessStartInfo(executable) { WorkingDirectory = state, CreateNoWindow = true };
            foreach (var name in start.Environment.Keys.ToArray())
                if (name.StartsWith("HOMELAB_", StringComparison.OrdinalIgnoreCase) || name.StartsWith("KOMBIFY_", StringComparison.OrdinalIgnoreCase)
                    || name.StartsWith("PG", StringComparison.OrdinalIgnoreCase) || name.StartsWith("AUTH0_", StringComparison.OrdinalIgnoreCase)
                    || name.StartsWith("IONOS_", StringComparison.OrdinalIgnoreCase) || name.StartsWith("CENTRON_", StringComparison.OrdinalIgnoreCase)
                    || name.StartsWith("SENTRY_", StringComparison.OrdinalIgnoreCase) || name.StartsWith("OTEL_", StringComparison.OrdinalIgnoreCase)
                    || name is "DATABASE_URL" or "ENVIRONMENT") start.Environment.Remove(name);
            start.Environment["HOMELAB_LISTEN_ADDR"] = origin.Authority;
            start.Environment["HOMELAB_GRPC_ADDR"] = "127.0.0.1:" + WalletProbePort();
            start.Environment["HOMELAB_DATA_DIR"] = dataDirectory;
            start.Environment["HOMELAB_RUNTIME_LOG_PATH"] = Path.Combine(dataDirectory, "runtime-logs.jsonl");
            start.Environment["HOMELAB_PUBLIC_ORIGIN"] = origin.GetLeftPart(UriPartial.Authority);
            start.Environment["HOMELAB_CORS_ORIGINS"] = origin.GetLeftPart(UriPartial.Authority);
            start.Environment["HOMELAB_ENV"] = "local";
            start.Environment["KOMBIFY_EDITION"] = "selfhost-oss";
            start.Environment["HOMELAB_EMBEDDED_POSTGRES"] = "1";
            start.Environment["HOMELAB_SHUTDOWN_ON_STDIN_CLOSE"] = "1";
            start.Environment["HOMELAB_EMBEDDED_POSTGRES_PORT"] = postgresPort.ToString();
            start.Environment["HOMELAB_EMBEDDED_POSTGRES_BUNDLE_DIR"] = ownBundle;
            start.Environment["HOMELAB_V2_SESSION_SECRET"] = signing;
            start.Environment["HOMELAB_ENCRYPTION_KEY"] = encryption;
            start.Environment["HOMELAB_V2_SESSION_AUDIENCE"] = "homelab-local";
            start.Environment["HOMELAB_V2_DEFAULT_TENANT_ID"] = "default";
            start.Environment["HOMELAB_LOCAL_DEVICE_TOKEN"] = device;
            start.Environment["HOMELAB_CLIENT_STATE_DIR"] = state;
            start.Environment["HOMELAB_ALLOW_UNSIGNED_WORKER_TOKEN"] = "1";
            runtime.Start(start, _ => { }, shutdownOnInputClose: true);
            using var readiness = new HttpClient(new HttpClientHandler { AllowAutoRedirect = false, UseCookies = false }) { Timeout = TimeSpan.FromSeconds(2) };
            var deadline = DateTime.UtcNow.AddSeconds(120);
            while (DateTime.UtcNow < deadline)
            {
                if (runtime.HasExited) throw new InvalidOperationException("Isolated Wallet runtime stopped at startup.");
                try
                {
                    using var response = await readiness.GetAsync(new Uri(origin, "/api/v1/info"));
                    if (response.IsSuccessStatusCode)
                    {
                        using var info = System.Text.Json.JsonDocument.Parse(await response.Content.ReadAsStringAsync());
                        var data = info.RootElement.GetProperty("data");
                        if (data.GetProperty("environment").GetString() != "local" || data.GetProperty("database_backend").GetString() != "postgres"
                            || data.GetProperty("edition").GetString() != "selfhost-oss") throw new InvalidOperationException("Wallet probe runtime posture does not match the isolated local PostgreSQL edition.");
                        if (expectedRevision is not null)
                        {
                            if (data.GetProperty("revision").GetString() != expectedRevision)
                                throw new InvalidOperationException("Wallet runtime source does not match the committed probe.");
                        }
                        return;
                    }
                }
                catch (HttpRequestException) { }
                catch (TaskCanceledException) { }
                await Task.Delay(500);
            }
            throw new InvalidOperationException("Isolated Wallet runtime did not become ready.");
        }
        await Start();
        using var transport = new WalletProbeTransport();
        using var http = new HttpClient(transport) { Timeout = TimeSpan.FromSeconds(30) };
        async Task<LocalWalletSession> Authenticate()
        {
            var cookies = await LocalDeviceSessionProof.AuthenticateAsync(http, origin, device);
            string Cookie(string name) => cookies.Single(value => value.StartsWith(name + "=", StringComparison.Ordinal)).Split(';')[0][(name.Length + 1)..];
            var session = new LocalWalletSession(Cookie("homelab_session"), Cookie("homelab_session_origin"), "");
            using var request = new HttpRequestMessage(HttpMethod.Get, new Uri(origin, "/api/v1/csrf"));
            request.Headers.TryAddWithoutValidation("Cookie", session.CookieHeader(""));
            using var response = await http.SendAsync(request);
            return session with { Csrf = response.Headers.GetValues("X-CSRF-Token").Single() };
        }
        var current = await Authenticate();
        async Task<System.Text.Json.JsonDocument> Api(HttpMethod method, string path, object? body = null)
        {
            using var request = new HttpRequestMessage(method, new Uri(origin, path));
            request.Headers.TryAddWithoutValidation("Cookie", current.CookieHeader(current.Csrf));
            request.Headers.TryAddWithoutValidation("X-CSRF-Token", current.Csrf);
            if (body is not null) request.Content = new StringContent(System.Text.Json.JsonSerializer.Serialize(body), System.Text.Encoding.UTF8, "application/json");
            using var response = await http.SendAsync(request);
            if (!response.IsSuccessStatusCode) throw new InvalidOperationException("Wallet probe API denied " + method + " " + path + " (" + (int)response.StatusCode + ").");
            return System.Text.Json.JsonDocument.Parse(await response.Content.ReadAsStringAsync());
        }
        var secret = "synthetic-wallet-" + Guid.NewGuid().ToString("N");
        using var created = await Api(HttpMethod.Post, "/api/v1/wallet", new { name = "Native Wallet boundary probe", kind = "password", secret, revealable = true });
        var assignedItem = created.RootElement.GetProperty("data").GetProperty("id").GetString()!;
        // Fixture setup uses a canonical-style opaque ID, not a generator
        // coverage claim. Owner, tenant and encrypted values remain unchanged.
        var item = assignedItem + ":pocketid:owner";
        StopWalletProbe(runtime, dataDirectory);
        await WalletProbeClosed(origin);
        await SeedOpaqueWalletProbeId(dataDirectory, assignedItem, item, "device:" + LocalWalletRevealProof.Hash(device));
        await Start();
        current = await Authenticate();
        Task<LocalWalletResult?> Reveal(string reason, Func<LocalWalletChallenge, CancellationToken, Task<bool>> confirmation,
            Func<CancellationToken, Task<LocalWalletSession?>>? sessionCheck = null) => LocalWalletRevealProof.RevealAsync(http, origin, device,
                current, item, reason, confirmation, sessionCheck ?? (_ => Task.FromResult<LocalWalletSession?>(current)));
        if (await Reveal("cancelled-probe", (_, _) => Task.FromResult(false)) is not null) throw new InvalidOperationException("Native cancellation disclosed a value.");
        async Task Denied(Func<Task<LocalWalletResult?>> operation)
        {
            try { await operation(); }
            catch (InvalidOperationException) { return; }
            throw new InvalidOperationException("Wallet boundary allowed an invalid disclosure.");
        }
        await Denied(() => Reveal("stale-document-probe", (_, _) => Task.FromResult(true), _ => Task.FromResult<LocalWalletSession?>(null)));
        await Denied(() => Reveal("changed-cookie-probe", (_, _) => Task.FromResult(true), _ => Task.FromResult<LocalWalletSession?>(current with { Origin = "changed" })));
        transport.MalformedChallenge = true;
        await Denied(() => Reveal("malformed-reply-probe", (_, _) => throw new InvalidOperationException("Malformed challenge reached native confirmation.")));
        transport.MalformedChallenge = false;
        await Denied(() => Reveal("rotated-during-dialog-probe", async (_, _) =>
        {
            using var changed = await Api(HttpMethod.Patch, "/api/v1/wallet/" + item, new { name = "Renamed while confirming", secret });
            return true;
        }));
        transport.TamperResult = true;
        await Denied(() => Reveal("tampered-result-probe", (_, _) => Task.FromResult(true)));
        transport.TamperResult = false;
        var revealed = await Reveal("windows-local-proof-probe", (_, _) => Task.FromResult(true));
        if (revealed?.Id != item || revealed.Secret != secret) throw new InvalidOperationException("Native authenticated reveal returned a different value.");
        using (var replay = await transport.Replay(http))
            if (replay.StatusCode != System.Net.HttpStatusCode.Forbidden) throw new InvalidOperationException("Wallet proof replay was accepted.");
        async Task AssertAudit()
        {
            using var activity = await Api(HttpMethod.Get, "/api/v1/activity?limit=100");
            var events = activity.RootElement.GetProperty("data").GetProperty("items").EnumerateArray().ToArray();
            bool Matches(System.Text.Json.JsonElement entry, string reason) => entry.GetProperty("action").GetString() == "wallet_reveal"
                && entry.GetProperty("details").GetProperty("resource_id").GetString() == item
                && entry.GetProperty("details").GetProperty("reason").GetString() == reason;
            if (!events.Any(entry => Matches(entry, "windows-local-proof-probe")) || events.Any(entry => Matches(entry, "cancelled-probe")))
                throw new InvalidOperationException("Wallet disclosure audit does not match confirmed/cancelled effects.");
        }
        await AssertAudit();
        runtime.Stop();
        await WalletProbeClosed(origin);
        if (File.Exists(Path.Combine(dataDirectory, "postgres", "data", "postmaster.pid")))
            throw new InvalidOperationException("Normal desktop shutdown left embedded PostgreSQL running.");
        await Start();
        current = await Authenticate();
        using var retained = await Api(HttpMethod.Get, "/api/v1/wallet");
        if (!retained.RootElement.GetProperty("data").GetProperty("items").EnumerateArray().Any(entry => entry.GetProperty("id").GetString() == item))
            throw new InvalidOperationException("Device-owned Wallet data was lost across restart.");
        await AssertAudit();
        var afterRestart = await Reveal("windows-local-restart-probe", (_, _) => Task.FromResult(true));
        if (afterRestart?.Secret != secret) throw new InvalidOperationException("Retained Wallet custody failed after restart.");
        using var runtimeBytes = File.OpenRead(executable);
        Console.WriteLine("PASS WindowsLocalWalletDeviceRevealBoundary production native transport/API/PostgreSQL/restart; runtime_sha256="
            + Convert.ToHexString(System.Security.Cryptography.SHA256.HashData(runtimeBytes)).ToLowerInvariant() + "; source=" + (expectedRevision ?? "uncommitted"));
    }
    finally
    {
        var cleanupErrors = new List<Exception>();
        try { StopWalletProbe(runtime, dataDirectory); await WalletProbeClosed(origin); } catch (Exception error) { cleanupErrors.Add(error); }
        foreach (var target in credentialTargets)
            try { WindowsCredentialStore.Delete(target); } catch (Exception error) { cleanupErrors.Add(error); }
        try { Environment.SetEnvironmentVariable("HOMELAB_CLIENT_STATE_DIR", previousState); } catch (Exception error) { cleanupErrors.Add(error); }
        var fullState = Path.GetFullPath(state);
        if (cleanupErrors.Count == 0 && fullState.StartsWith(Path.GetFullPath(root) + Path.DirectorySeparatorChar, StringComparison.OrdinalIgnoreCase)
            && Path.GetFileName(fullState).StartsWith("wallet-live-probe-", StringComparison.Ordinal) && Directory.Exists(fullState)) Directory.Delete(fullState, true);
        if (cleanupErrors.Count != 0) throw new AggregateException("Wallet probe cleanup failed; isolated state was preserved.", cleanupErrors);
    }
}

static int WalletProbePort()
{
    var listener = new System.Net.Sockets.TcpListener(System.Net.IPAddress.Loopback, 0);
    listener.Start();
    var port = ((System.Net.IPEndPoint)listener.LocalEndpoint).Port;
    listener.Stop();
    return port;
}

static async Task WalletProbeClosed(Uri origin)
{
    var deadline = DateTime.UtcNow.AddSeconds(5);
    while (DateTime.UtcNow < deadline)
    {
        using var socket = new System.Net.Sockets.TcpClient();
        try { await socket.ConnectAsync(System.Net.IPAddress.Loopback, origin.Port); }
        catch (System.Net.Sockets.SocketException) { return; }
        await Task.Delay(100);
    }
    throw new InvalidOperationException("Owned Wallet HTTP listener did not close; fixture preserved.");
}

static void StopWalletProbe(SupervisedProcess runtime, string dataDirectory)
{
    var binaries = Path.Combine(dataDirectory, "postgres", "runtime");
    var pidPath = Path.Combine(dataDirectory, "postgres", "data", "postmaster.pid");
    using var postgres = OwnedWalletProbePostgres(dataDirectory);
    runtime.Stop();
    if (postgres is null || postgres.HasExited) return;
    if (!File.Exists(pidPath) || File.ReadLines(pidPath).FirstOrDefault() != postgres.Id.ToString())
        throw new InvalidOperationException("Owned PostgreSQL PID changed; fixture preserved.");
    var pgctl = Directory.EnumerateFiles(binaries, "pg_ctl.exe", SearchOption.AllDirectories).FirstOrDefault();
    if (pgctl is null) throw new InvalidOperationException("Owned PostgreSQL stop tool missing; fixture preserved.");
    var start = new System.Diagnostics.ProcessStartInfo(pgctl) { CreateNoWindow = true, UseShellExecute = false,
        RedirectStandardOutput = true, RedirectStandardError = true };
    foreach (var argument in new[] { "stop", "-D", Path.Combine(dataDirectory, "postgres", "data"), "-m", "fast", "-w", "-t", "10" }) start.ArgumentList.Add(argument);
    using var stop = System.Diagnostics.Process.Start(start);
    if (stop is null) throw new InvalidOperationException("Owned PostgreSQL stop did not start; fixture preserved.");
    if (!stop.WaitForExit(12000)) { stop.Kill(true); stop.WaitForExit(); throw new InvalidOperationException("Owned PostgreSQL stop timed out; fixture preserved."); }
    if (stop.ExitCode != 0 || !postgres.WaitForExit(3000)) throw new InvalidOperationException("Owned PostgreSQL did not stop; fixture preserved.");
}

static System.Diagnostics.Process? OwnedWalletProbePostgres(string dataDirectory)
{
    var binaries = Path.Combine(dataDirectory, "postgres", "runtime");
    var pidPath = Path.Combine(dataDirectory, "postgres", "data", "postmaster.pid");
    if (!File.Exists(pidPath)) return null;
    if (!int.TryParse(File.ReadLines(pidPath).FirstOrDefault(), out var pid)) throw new InvalidOperationException("Invalid owned PostgreSQL PID; fixture preserved.");
    System.Diagnostics.Process postgres;
    try { postgres = System.Diagnostics.Process.GetProcessById(pid); }
    catch (ArgumentException) { return null; }
    try
    {
        if (!postgres.HasExited)
        {
            var image = postgres.MainModule?.FileName;
            if (image is null || !Path.GetFullPath(image).StartsWith(Path.GetFullPath(binaries) + Path.DirectorySeparatorChar, StringComparison.OrdinalIgnoreCase)
                || Path.GetFileName(image) != "postgres.exe") throw new InvalidOperationException("PostgreSQL ownership could not be verified; fixture preserved.");
        }
        return postgres;
    }
    catch { postgres.Dispose(); throw; }
}

static async Task SeedOpaqueWalletProbeId(string dataDirectory, string assignedId, string opaqueId, string owner)
{
    if (!Guid.TryParse(assignedId, out _) || opaqueId != assignedId + ":pocketid:owner"
        || owner.Length != 71 || !owner.StartsWith("device:", StringComparison.Ordinal)
        || !owner[7..].All(c => c is >= '0' and <= '9' or >= 'a' and <= 'f')) throw new InvalidOperationException("Unsafe opaque Wallet fixture identity.");
    using var postgres = OwnedWalletProbePostgres(dataDirectory);
    if (postgres is not null && !postgres.HasExited) throw new InvalidOperationException("Owned Wallet fixture PostgreSQL must be stopped before seed.");
    var binaries = Path.Combine(dataDirectory, "postgres", "runtime");
    var executable = Directory.EnumerateFiles(binaries, "postgres.exe", SearchOption.AllDirectories).FirstOrDefault()
        ?? throw new InvalidOperationException("Owned Wallet fixture database helper unavailable.");
    var start = new System.Diagnostics.ProcessStartInfo(executable) { WorkingDirectory = dataDirectory, CreateNoWindow = true, UseShellExecute = false,
        RedirectStandardInput = true, RedirectStandardOutput = true, RedirectStandardError = true };
    foreach (var name in start.Environment.Keys.Where(name => name.StartsWith("PG", StringComparison.OrdinalIgnoreCase)).ToArray()) start.Environment.Remove(name);
    // Existing PostgreSQL single-user primitive; first flag and final database
    // are prescribed by PostgreSQL. No client dependency or catalog changes.
    foreach (var argument in new[] { "--single", "-D", Path.Combine(dataDirectory, "postgres", "data"), "homelab" }) start.ArgumentList.Add(argument);
    using var seed = System.Diagnostics.Process.Start(start) ?? throw new InvalidOperationException("Wallet fixture seed did not start.");
    var stdout = seed.StandardOutput.ReadToEndAsync();
    var stderr = seed.StandardError.ReadToEndAsync();
    await seed.StandardInput.WriteLineAsync("BEGIN; SELECT set_config('app.tenant_id','default',true); UPDATE wallet_items SET id='" + opaqueId + "', metadata_json=jsonb_set(metadata_json, '{id}', to_jsonb('"
        + opaqueId + "'::text), true) WHERE id='" + assignedId + "' AND tenant_id='default' AND metadata_json->>'owner_id'='" + owner + "'; COMMIT;");
    seed.StandardInput.Close();
    using var timeout = new CancellationTokenSource(TimeSpan.FromSeconds(10));
    try { await seed.WaitForExitAsync(timeout.Token); }
    catch (OperationCanceledException) { seed.Kill(true); await seed.WaitForExitAsync(); throw new InvalidOperationException("Owned Wallet fixture seed timed out."); }
    await Task.WhenAll(stdout, stderr);
    if (seed.ExitCode != 0 || stderr.Result.Contains("ERROR:", StringComparison.Ordinal)) throw new InvalidOperationException("Owned Wallet fixture seed failed.");
}

sealed class WalletProbeTransport : DelegatingHandler
{
    internal bool MalformedChallenge, TamperResult;
    private Uri? replayUri;
    private string? replayBody;
    private string? replayCookies;
    private string? replayCsrf;
    internal WalletProbeTransport() : base(new HttpClientHandler { AllowAutoRedirect = false, UseCookies = false }) { }
    protected override async Task<HttpResponseMessage> SendAsync(HttpRequestMessage request, CancellationToken ct)
    {
        if (MalformedChallenge && request.RequestUri!.AbsolutePath.EndsWith("/device-reauth/challenge", StringComparison.Ordinal))
            return new HttpResponseMessage(System.Net.HttpStatusCode.OK) { Content = new StringContent("{\"data\":{}}") };
        if (request.RequestUri!.AbsolutePath.EndsWith("/reveal", StringComparison.Ordinal))
        {
            replayUri = request.RequestUri;
            replayBody = await request.Content!.ReadAsStringAsync(ct);
            replayCookies = request.Headers.GetValues("Cookie").Single();
            replayCsrf = request.Headers.GetValues("X-CSRF-Token").Single();
        }
        var response = await base.SendAsync(request, ct);
        if (TamperResult && response.IsSuccessStatusCode && request.RequestUri.AbsolutePath.EndsWith("/reveal", StringComparison.Ordinal))
        { response.Content = new StringContent("{\"data\":{\"payload\":\"e30\",\"proof\":\"" + new string('0', 64) + "\"}}"); }
        return response;
    }
    internal async Task<HttpResponseMessage> Replay(HttpClient http)
    {
        using var request = new HttpRequestMessage(HttpMethod.Post, replayUri) { Content = new StringContent(replayBody!, System.Text.Encoding.UTF8, "application/json") };
        request.Headers.TryAddWithoutValidation("Cookie", replayCookies);
        request.Headers.TryAddWithoutValidation("X-CSRF-Token", replayCsrf);
        return await http.SendAsync(request);
    }
}

#if CLOUD_DESKTOP
/// <summary>Auth0's token endpoint scripted answer by answer (the last repeats); an exception answers as a network fault or timeout.</summary>
sealed class ScriptedAuth0(IReadOnlyList<object> answers) : HttpMessageHandler
{
    internal sealed record Answer(System.Net.HttpStatusCode Status, string Body, int? RetryAfterSeconds = null);

    /// <summary>A send that stays in flight until the caller cancels it; <see cref="Sent"/> says it reached the handler.</summary>
    internal sealed class InFlight
    {
        internal TaskCompletionSource Sent { get; } = new(TaskCreationOptions.RunContinuationsAsynchronously);
    }

    private int next;
    internal List<string> RefreshTokens { get; } = [];
    protected override async Task<HttpResponseMessage> SendAsync(HttpRequestMessage request, CancellationToken cancellationToken)
    {
        var form = System.Web.HttpUtility.ParseQueryString(await request.Content!.ReadAsStringAsync(cancellationToken));
        RefreshTokens.Add(form["refresh_token"] ?? "");
        var step = answers[Math.Min(next++, answers.Count - 1)];
        if (step is InFlight inFlight)
        {
            inFlight.Sent.TrySetResult();
            await Task.Delay(Timeout.InfiniteTimeSpan, cancellationToken);
        }
        if (step is Exception fault) throw fault;
        var answer = (Answer)step;
        var response = new HttpResponseMessage(answer.Status) { Content = new StringContent(answer.Body) };
        if (answer.RetryAfterSeconds is { } seconds) response.Headers.RetryAfter = new System.Net.Http.Headers.RetryConditionHeaderValue(TimeSpan.FromSeconds(seconds));
        return response;
    }
}
#endif
