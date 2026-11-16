using System.Diagnostics;
using System.Reflection;
using System.Security.Cryptography;
using System.Text;
using System.Text.Json;
using Kombify.Client.Shell;

namespace Kombify.TechStack.Client;

internal sealed record ConnectProductIdentity(string Subject, string Tenant);
internal sealed class ConnectAccountException : Exception { }

/// <summary>
/// Registers this Techstack Windows client as a kombify Connect installation
/// (<c>app_id techstack-windows</c>) so the Techstack dashboard's device view
/// can show it next to the other kombify apps on this machine. Registration
/// alone grants nothing: the installation stays <c>pending</c> until an active
/// installation of the same account confirms it (contract section 5).
/// </summary>
internal static class ConnectEnrollment
{
    internal const string AppId = "techstack-windows";
    private const string KeyName = "kombify-techstack-client-connect-dpop";
    // Fixed loopback callbacks registered on the Auth0 native application.
    private static readonly int[] LoopbackPorts = [63690, 63691, 63692, 63693, 63694];

    internal static bool Configured(ClientConfig config) => !string.IsNullOrWhiteSpace(config.ConnectAuth0ClientId);

    private static string SubjectHash(string subject) => Convert.ToHexString(
        SHA256.HashData(Encoding.UTF8.GetBytes(subject)));

    internal static string RefreshTarget(string subject) =>
        ClientConfig.CredentialTarget("connect-refresh-token/" + SubjectHash(subject));

    private static string InstallationPath(string subject) => Path.Combine(
        ClientConfig.StateDirectory(), "connect-installations", SubjectHash(subject) + ".json");

    internal static void ShowFailure(Exception ex)
    {
        if (ex is ConnectAccountException)
        {
            MessageBox.Show(CloudLoginText.Get("ConnectAccountRequired"), "kombify Connect",
                MessageBoxButtons.OK, MessageBoxIcon.Warning);
            return;
        }
        if (ex is ConnectDenialException denial)
        {
            MessageBox.Show($"{denial.Title}\n\n{denial.Body}", "kombify Connect", MessageBoxButtons.OK, MessageBoxIcon.Warning);
            return;
        }

        MessageBox.Show($"kombify Connect request failed.\n\n{ex.Message}", "kombify Connect",
            MessageBoxButtons.OK, MessageBoxIcon.Error);
    }

    private static readonly object PossiblyRetiredLock = new();
    private static readonly Dictionary<string, string> PossiblyRetiredTokens = new(StringComparer.Ordinal);

    internal static HttpClient NewHttpClient() =>
        new(new HttpClientHandler { AllowAutoRedirect = false }) { Timeout = TimeSpan.FromSeconds(15) };

    /// <summary>
    /// Uses the stored refresh token when there is one; otherwise, and when Auth0 refuses it for good,
    /// signs in through Auth0 in the browser. A temporary failure (429, 5xx, network, timeout) is retried
    /// within the 25 s renewal budget and then reported, with the stored token kept: it never becomes a sign-in.
    /// If a send may have retired the stored token (a timeout, a connection lost after the request was written, an
    /// answer without a usable token, or a send the caller cancelled in flight), nothing more is sent in this run and
    /// "not ready" is reported; the next start tries again. An operator fault (<c>invalid_client</c>,
    /// <c>unauthorized_client</c>) is reported as "not ready" the same way. The access token is never persisted.
    /// </summary>
    internal static async Task<Auth0NativeSession> SignInForAccountAsync(
        ClientConfig config, HttpClient http, string subject, CancellationToken cancellationToken)
    {
        if (!Configured(config))
        {
            throw new InvalidOperationException(
                "kombify Connect is not configured for this client yet (no Auth0 native client id).");
        }

        var login = new Auth0NativeLogin(
            http,
            new Uri(config.ConnectAuth0Issuer),
            config.ConnectAuth0ClientId,
            config.ConnectAuth0Audience,
            LoopbackPorts);
        Auth0NativeSession? session = null;
        var target = RefreshTarget(subject);
        var stored = WindowsCredentialStore.Read(target);
        if (!string.IsNullOrWhiteSpace(stored))
        {
            lock (PossiblyRetiredLock)
            {
                if (PossiblyRetiredTokens.TryGetValue(target, out var retired) && retired == stored)
                {
                    throw Auth0RefreshUnavailableException.NotReadyForTimedOutToken();
                }
            }

            try
            {
                session = await login.RefreshWithBackoffAsync(stored, cancellationToken);
                // Persist a rotated token immediately, before any caller account check or cancellation can discard it.
                if (session.RefreshToken != stored) SaveRefreshForAccount(subject, session);
                lock (PossiblyRetiredLock) PossiblyRetiredTokens.Remove(target);
            }
            catch (Auth0RefreshUnavailableException ex) when (ex.PossiblyRetired)
            {
                lock (PossiblyRetiredLock) PossiblyRetiredTokens[target] = stored;
                throw;
            }
            catch (ConnectRefreshCancelledException)
            {
                // The caller stopped the send in flight: Auth0 may have received it, so nothing more is sent in this run.
                lock (PossiblyRetiredLock) PossiblyRetiredTokens[target] = stored;
                throw;
            }
            catch (Auth0GrantRefusedException)
            {
                lock (PossiblyRetiredLock) PossiblyRetiredTokens.Remove(target);
                // Auth0 refused the grant for good (revoked or expired refresh token): fall through to an interactive sign-in.
            }
        }

        session ??= await login.SignInAsync(
            url => Process.Start(new ProcessStartInfo { FileName = url.ToString(), UseShellExecute = true }),
            cancellationToken);
        return session;
    }

    internal static async Task<T> WithVerifiedSessionAsync<T>(
        ConnectProductIdentity expected,
        Func<CancellationToken, Task<ConnectProductIdentity>> currentIdentity,
        Func<CancellationToken, Task<Auth0NativeSession>> signIn,
        Action<Auth0NativeSession> saveRefresh,
        Func<Auth0NativeSession, CancellationToken, Task<T>> operation,
        CancellationToken token)
    {
        await RequireCurrentAccountAsync(expected, currentIdentity, token);
        var session = await signIn(token);
        if (session.SubjectId != expected.Subject) throw new ConnectAccountException();
        saveRefresh(session);
        await RequireCurrentAccountAsync(expected, currentIdentity, token);
        return await operation(session, token);
    }

    internal static async Task RequireCurrentAccountAsync(ConnectProductIdentity expected,
        Func<CancellationToken, Task<ConnectProductIdentity>> currentIdentity, CancellationToken token)
    {
        token.ThrowIfCancellationRequested();
        if (await currentIdentity(token) != expected) throw new ConnectAccountException();
        token.ThrowIfCancellationRequested();
    }

    internal static void SaveRefreshForAccount(string subject, Auth0NativeSession session)
    {
        if (string.IsNullOrWhiteSpace(session.RefreshToken)) return;
        var target = RefreshTarget(subject);
        if (WindowsCredentialStore.Read(target) == session.RefreshToken) return;
        // Read the store back once: a store that kept the old value would replay a retired token at the next start.
        for (var attempt = 1; attempt <= 2; attempt++)
        {
            WindowsCredentialStore.Write(target, subject, session.RefreshToken);
            if (WindowsCredentialStore.Read(target) == session.RefreshToken) return;
            System.Diagnostics.Trace.TraceWarning("kombify Connect: the credential store kept its old refresh token after a save" +
                (attempt == 1 ? "; saving again." : "; still the old value."));
        }
    }

    internal static ConnectDeviceKey OpenKey() => ConnectDeviceKey.OpenOrCreate(KeyName);

    internal static ConnectEnrollmentClient Client(ClientConfig config, HttpClient http) =>
        new(http, new Uri(config.ConnectOrigin));

    public static async Task<ConnectInstallation> EnrollAsync(ClientConfig config,
        ConnectProductIdentity identity, Func<CancellationToken, Task<ConnectProductIdentity>> currentIdentity,
        CancellationToken cancellationToken)
    {
        using var http = NewHttpClient();
        var installation = await WithVerifiedSessionAsync(identity, currentIdentity,
            token => SignInForAccountAsync(config, http, identity.Subject, token),
            session => SaveRefreshForAccount(identity.Subject, session),
            async (session, token) =>
            {
                using var key = OpenKey();
                return await Client(config, http).RegisterAsync(session.AccessToken, key,
                    AppId, $"Techstack on {Environment.MachineName}", AppVersion(),
                    ConnectDeviceRef.Compute(identity.Subject), token);
            }, cancellationToken);

        await RequireCurrentAccountAsync(identity, currentIdentity, cancellationToken);
        var path = InstallationPath(identity.Subject);
        Directory.CreateDirectory(Path.GetDirectoryName(path)!);
        File.WriteAllText(
            path,
            JsonSerializer.Serialize(new
            {
                subjectHash = SubjectHash(identity.Subject),
                installationId = installation.InstallationId,
                status = installation.Status,
                custodyClass = installation.CustodyClass,
                keyName = KeyName,
                registeredAt = DateTimeOffset.UtcNow,
            }, new JsonSerializerOptions { WriteIndented = true }));
        return installation;
    }

    internal static string? RegisteredInstallationId(string subject)
    {
        var path = InstallationPath(subject);
        if (!File.Exists(path))
        {
            return null;
        }

        using var document = JsonDocument.Parse(File.ReadAllText(path));
        if (!document.RootElement.TryGetProperty("subjectHash", out var hash)
            || hash.GetString() != SubjectHash(subject)) throw new ConnectAccountException();
        return document.RootElement.TryGetProperty("installationId", out var id) ? id.GetString() : null;
    }

    private static string AppVersion()
    {
        var informational = Assembly.GetEntryAssembly()?
            .GetCustomAttribute<AssemblyInformationalVersionAttribute>()?.InformationalVersion ?? "0.0.0-dev";
        // Connect version_string: no build metadata beyond the allowed characters.
        var version = new string(informational.TakeWhile(ch => char.IsLetterOrDigit(ch) || ch is '.' or '-' or '_').ToArray());
        return string.IsNullOrEmpty(version) ? "0.0.0-dev" : version;
    }
}

/// <summary>
/// "kombify Connect" settings, opened from the window's system menu: registers this PC and,
/// once this installation is active, confirms the account's other pending installations.
/// </summary>
internal sealed class ConnectSettingsForm : Form
{
    private readonly ClientConfig _config;
    private readonly ConnectProductIdentity _identity;
    private readonly Func<CancellationToken, Task<ConnectProductIdentity>> _currentIdentity;
    private readonly CancellationTokenSource _closed = new();
    private readonly Label _status = new() { Dock = DockStyle.Top, Height = 56, Padding = new Padding(8) };
    private readonly ListView _installations = new()
    {
        Dock = DockStyle.Fill,
        View = View.Details,
        FullRowSelect = true,
        MultiSelect = false,
    };
    private readonly Button _register = new() { Text = "Register this PC", AutoSize = true };
    private readonly Button _refresh = new() { Text = "Refresh", AutoSize = true };
    private readonly Button _confirm = new() { Text = "Confirm selected", AutoSize = true, Enabled = false };

    public ConnectSettingsForm(ClientConfig config, ConnectProductIdentity identity,
        Func<CancellationToken, Task<ConnectProductIdentity>> currentIdentity)
    {
        _config = config;
        _identity = identity;
        _currentIdentity = currentIdentity;
        Text = "kombify Connect";
        Size = new Size(720, 420);
        StartPosition = FormStartPosition.CenterParent;
        _installations.Columns.Add("App", 160);
        _installations.Columns.Add("Name", 280);
        _installations.Columns.Add("Status", 100);
        _installations.Columns.Add("Platform", 100);
        var buttons = new FlowLayoutPanel { Dock = DockStyle.Bottom, Height = 44, Padding = new Padding(6) };
        buttons.Controls.AddRange([_register, _refresh, _confirm]);
        Controls.Add(_installations);
        Controls.Add(buttons);
        Controls.Add(_status);

        _register.Click += async (_, _) => await RunAsync(async token =>
        {
            await ConnectEnrollment.EnrollAsync(_config, _identity, _currentIdentity, token);
            await LoadAsync(token);
        });
        _refresh.Click += async (_, _) => await RunAsync(LoadAsync);
        _confirm.Click += async (_, _) => await RunAsync(ConfirmSelectedAsync);
        _installations.SelectedIndexChanged += (_, _) => UpdateConfirm();
        Shown += (_, _) => Describe();
        FormClosed += (_, _) => _closed.Cancel();
    }

    private bool _thisActive;

    private void Describe()
    {
        var id = ConnectEnrollment.RegisteredInstallationId(_identity.Subject);
        _status.Text = !ConnectEnrollment.Configured(_config)
            ? "kombify Connect is not configured for this client."
            : id is null
                ? "This PC is not registered with kombify Connect yet."
                : $"This PC is registered ({id}). Refresh to see its status and your other installations.";
        _register.Enabled = ConnectEnrollment.Configured(_config);
        _refresh.Enabled = id is not null;
    }

    private void UpdateConfirm()
    {
        _confirm.Enabled = _thisActive
            && _installations.SelectedItems.Count == 1
            && (_installations.SelectedItems[0].Tag as ConnectInstallation)?.Status == "pending";
    }

    private async Task LoadAsync(CancellationToken token)
    {
        using var http = ConnectEnrollment.NewHttpClient();
        var installations = await ConnectEnrollment.WithVerifiedSessionAsync(_identity, _currentIdentity,
            ct => ConnectEnrollment.SignInForAccountAsync(_config, http, _identity.Subject, ct),
            session => ConnectEnrollment.SaveRefreshForAccount(_identity.Subject, session),
            async (session, ct) =>
            {
                using var key = ConnectEnrollment.OpenKey();
                return await ConnectEnrollment.Client(_config, http).ListInstallationsAsync(session.AccessToken, key, ct);
            }, token);
        await ConnectEnrollment.RequireCurrentAccountAsync(_identity, _currentIdentity, token);
        var ownId = ConnectEnrollment.RegisteredInstallationId(_identity.Subject);
        _thisActive = installations.Any(entry => entry.InstallationId == ownId && entry.Status == "active");
        _installations.Items.Clear();
        foreach (var entry in installations)
        {
            var item = new ListViewItem([entry.AppId, entry.DisplayName, entry.Status, entry.Platform]) { Tag = entry };
            _installations.Items.Add(item);
        }

        var own = installations.FirstOrDefault(entry => entry.InstallationId == ownId);
        _status.Text = own is null
            ? "This PC is not registered with kombify Connect yet."
            : own.Status == "active"
                ? "This PC is active. Select a pending installation of your account to confirm it."
                : $"This PC is {own.Status}. Confirm it from another active kombify app of your account.";
        UpdateConfirm();
    }

    private async Task ConfirmSelectedAsync(CancellationToken token)
    {
        if (_installations.SelectedItems.Count != 1 || _installations.SelectedItems[0].Tag is not ConnectInstallation target)
        {
            return;
        }

        using var http = ConnectEnrollment.NewHttpClient();
        await ConnectEnrollment.WithVerifiedSessionAsync(_identity, _currentIdentity,
            ct => ConnectEnrollment.SignInForAccountAsync(_config, http, _identity.Subject, ct),
            session => ConnectEnrollment.SaveRefreshForAccount(_identity.Subject, session),
            async (session, ct) =>
            {
                using var key = ConnectEnrollment.OpenKey();
                await ConnectEnrollment.Client(_config, http).ConfirmAsync(session.AccessToken, key,
                    target.InstallationId, ct);
                return true;
            }, token);
        await LoadAsync(token);
    }

    private async Task RunAsync(Func<CancellationToken, Task> action)
    {
        UseWaitCursor = true;
        Enabled = false;
        try
        {
            await ConnectEnrollment.RequireCurrentAccountAsync(_identity, _currentIdentity, _closed.Token);
            await action(_closed.Token);
        }
        catch (OperationCanceledException) when (_closed.IsCancellationRequested) { }
        catch (Exception ex)
        {
            if (!IsDisposed) ConnectEnrollment.ShowFailure(ex);
        }
        finally
        {
            if (!IsDisposed)
            {
                Enabled = true;
                UseWaitCursor = false;
            }
        }
    }
}
