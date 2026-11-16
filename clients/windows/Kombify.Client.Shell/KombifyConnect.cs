using System.Diagnostics;
using System.Net;
using System.Net.Http.Headers;
using System.Security.Cryptography;
using System.Text;
using System.Text.Json;
using System.Text.Json.Serialization;
using Microsoft.Win32;

namespace Kombify.Client.Shell;

// kombify Connect installation enrollment for native Windows clients
// (platform/standards/CONNECT-CONTRACT-STANDARD.md sections 2, 4 and 5).
// The client proves possession of a non-exportable P-256 key with DPoP on
// every Connect call; the key never leaves the Windows key storage provider.

/// <summary>Declared custody of the installation key (contract section 2, NATIVE-CLIENT-PLATFORM-STANDARD 5.1).</summary>
public enum ConnectCustodyClass
{
    Hardware,
    OsProtected,
}

/// <summary>
/// The installation's DPoP key: a persisted, non-exportable CNG key. The TPM-backed
/// Microsoft Platform Crypto Provider is tried first (hardware custody); without a
/// usable TPM the per-user Microsoft Software Key Storage Provider holds it, which
/// DPAPI protects at rest (os-protected custody). The class never upgrades a claim.
/// </summary>
public sealed class ConnectDeviceKey : IDisposable
{
    private static readonly CngProvider PlatformProvider = new("Microsoft Platform Crypto Provider");

    private readonly ECDsaCng _ecdsa;

    private ConnectDeviceKey(CngKey key, ConnectCustodyClass custody)
    {
        _ecdsa = new ECDsaCng(key);
        Custody = custody;
        var parameters = _ecdsa.ExportParameters(includePrivateParameters: false);
        X = Base64Url(parameters.Q.X!);
        Y = Base64Url(parameters.Q.Y!);
        // RFC 7638: members in lexicographic order, no whitespace.
        Thumbprint = Base64Url(SHA256.HashData(Encoding.UTF8.GetBytes(
            $"{{\"crv\":\"P-256\",\"kty\":\"EC\",\"x\":\"{X}\",\"y\":\"{Y}\"}}")));
    }

    public ConnectCustodyClass Custody { get; }
    public string X { get; }
    public string Y { get; }
    /// <summary>RFC 7638 JWK thumbprint, the Connect <c>dpop_jkt</c>.</summary>
    public string Thumbprint { get; }

    public static ConnectDeviceKey OpenOrCreate(string keyName)
    {
        if (!OperatingSystem.IsWindows())
        {
            throw new PlatformNotSupportedException("Windows CNG is required for the Connect installation key.");
        }

        foreach (var (provider, custody) in new[]
                 {
                     (PlatformProvider, ConnectCustodyClass.Hardware),
                     (CngProvider.MicrosoftSoftwareKeyStorageProvider, ConnectCustodyClass.OsProtected),
                 })
        {
            try
            {
                var key = CngKey.Exists(keyName, provider)
                    ? CngKey.Open(keyName, provider)
                    : CngKey.Create(CngAlgorithm.ECDsaP256, keyName, new CngKeyCreationParameters
                    {
                        Provider = provider,
                        ExportPolicy = CngExportPolicies.None,
                        KeyCreationOptions = CngKeyCreationOptions.None,
                    });
                return new ConnectDeviceKey(key, custody);
            }
            catch (CryptographicException) when (provider == PlatformProvider)
            {
                // No TPM 2.0 or the platform provider refused: fall back to os-protected custody.
            }
        }

        throw new CryptographicException("No Windows key storage provider could hold the Connect installation key.");
    }

    /// <summary>RFC 9449 DPoP proof for one request (ES256, <c>typ dpop+jwt</c>).</summary>
    public string CreateProof(string method, Uri url)
    {
        var header = JsonSerializer.Serialize(new
        {
            alg = "ES256",
            typ = "dpop+jwt",
            jwk = new { kty = "EC", crv = "P-256", x = X, y = Y },
        });
        var htu = url.GetLeftPart(UriPartial.Path);
        var payload = JsonSerializer.Serialize(new
        {
            htm = method.ToUpperInvariant(),
            htu,
            iat = DateTimeOffset.UtcNow.ToUnixTimeSeconds(),
            jti = Guid.NewGuid().ToString("N"),
        });
        var signingInput = $"{Base64Url(Encoding.UTF8.GetBytes(header))}.{Base64Url(Encoding.UTF8.GetBytes(payload))}";
        // .NET emits the IEEE P1363 r||s form that JWS ES256 requires.
        var signature = _ecdsa.SignData(Encoding.ASCII.GetBytes(signingInput), HashAlgorithmName.SHA256);
        return $"{signingInput}.{Base64Url(signature)}";
    }

    /// <summary>Removes the installation key from both providers (reset or revocation cleanup).</summary>
    public static void Delete(string keyName)
    {
        foreach (var provider in new[] { PlatformProvider, CngProvider.MicrosoftSoftwareKeyStorageProvider })
        {
            try
            {
                if (CngKey.Exists(keyName, provider))
                {
                    using var key = CngKey.Open(keyName, provider);
                    key.Delete();
                }
            }
            catch (CryptographicException)
            {
                // Provider unavailable on this machine.
            }
        }
    }

    /// <summary>Verifies a proof this key produced; used by the contract tests.</summary>
    public bool VerifyProof(string proof)
    {
        var lastDot = proof.LastIndexOf('.');
        var signature = Convert.FromBase64String(Pad(proof[(lastDot + 1)..].Replace('-', '+').Replace('_', '/')));
        return _ecdsa.VerifyData(Encoding.ASCII.GetBytes(proof[..lastDot]), signature, HashAlgorithmName.SHA256);
    }

    private static string Pad(string value) => value.PadRight(value.Length + (4 - value.Length % 4) % 4, '=');

    public void Dispose() => _ecdsa.Dispose();

    internal static string Base64Url(ReadOnlySpan<byte> bytes) =>
        Convert.ToBase64String(bytes).TrimEnd('=').Replace('+', '-').Replace('/', '_');
}

/// <summary>
/// The display-only <c>device_ref</c> (contract section 2): HMAC-SHA256 of the subject keyed
/// with the Windows MachineGuid. Every kombify app of one subject on this machine derives the
/// same value; the MachineGuid itself never leaves the machine.
/// </summary>
public static class ConnectDeviceRef
{
    public static string? Compute(string subjectId)
    {
        if (!OperatingSystem.IsWindows() || string.IsNullOrWhiteSpace(subjectId))
        {
            return null;
        }

        using var cryptography = RegistryKey.OpenBaseKey(RegistryHive.LocalMachine, RegistryView.Registry64)
            .OpenSubKey(@"SOFTWARE\Microsoft\Cryptography");
        if (cryptography?.GetValue("MachineGuid") is not string machineGuid || string.IsNullOrWhiteSpace(machineGuid))
        {
            return null;
        }

        var mac = HMACSHA256.HashData(
            Encoding.UTF8.GetBytes(machineGuid.Trim()),
            Encoding.UTF8.GetBytes($"kombify-connect-device:v1:{subjectId}"));
        return $"dev_{ConnectDeviceKey.Base64Url(mac)}";
    }
}

public sealed record ConnectInstallation
{
    [JsonPropertyName("installation_id")] public string InstallationId { get; init; } = "";
    [JsonPropertyName("subject_id")] public string SubjectId { get; init; } = "";
    [JsonPropertyName("app_id")] public string AppId { get; init; } = "";
    [JsonPropertyName("status")] public string Status { get; init; } = "";
    [JsonPropertyName("custody_class")] public string CustodyClass { get; init; } = "";
    [JsonPropertyName("device_ref")] public string? DeviceRef { get; init; }
    [JsonPropertyName("display_name")] public string DisplayName { get; init; } = "";
    [JsonPropertyName("platform")] public string Platform { get; init; } = "";
}

/// <summary>Structured Connect refusal (ConnectDenialV1); carries guidance, never secrets.</summary>
public sealed class ConnectDenialException(HttpStatusCode status, string reasonCode, string title, string body)
    : Exception($"{title}: {body} ({reasonCode})")
{
    public HttpStatusCode Status { get; } = status;
    public string ReasonCode { get; } = reasonCode;
    public string Title { get; } = title;
    public string Body { get; } = body;
}

/// <summary>Registers this app as a Connect installation (contract section 5).</summary>
public sealed class ConnectEnrollmentClient(HttpClient http, Uri connectOrigin)
{
    public async Task<ConnectInstallation> RegisterAsync(
        string accessToken,
        ConnectDeviceKey key,
        string appId,
        string displayName,
        string appVersion,
        string? deviceRef,
        CancellationToken cancellationToken)
    {
        var challenge = await SendAsync<ChallengeResponse>(
            HttpMethod.Post, "/v1/installations/challenges", accessToken, key, body: null, cancellationToken);
        var registration = new Dictionary<string, string>
        {
            ["challenge"] = challenge.Challenge,
            ["app_id"] = appId,
            ["platform"] = "windows",
            ["custody_class"] = key.Custody == ConnectCustodyClass.Hardware ? "hardware" : "os-protected",
            ["display_name"] = displayName.Length > 80 ? displayName[..80] : displayName,
            ["app_version"] = appVersion,
        };
        if (deviceRef is not null)
        {
            registration["device_ref"] = deviceRef;
        }

        return await SendAsync<ConnectInstallation>(
            HttpMethod.Post, "/v1/installations", accessToken, key, registration, cancellationToken);
    }

    /// <summary>The account's installations (contract section 5, step 3); needs a registered key.</summary>
    public async Task<IReadOnlyList<ConnectInstallation>> ListInstallationsAsync(
        string accessToken, ConnectDeviceKey key, CancellationToken cancellationToken)
    {
        var list = await SendAsync<InstallationList>(HttpMethod.Get, "/v1/installations", accessToken, key, null, cancellationToken);
        return list.Installations;
    }

    /// <summary>
    /// Confirms a pending installation of the same account. Gateway allows it only when this
    /// installation is active (contract section 5), so a fresh install cannot confirm itself.
    /// </summary>
    public Task<ConnectInstallation> ConfirmAsync(
        string accessToken, ConnectDeviceKey key, string installationId, CancellationToken cancellationToken) =>
        SendAsync<ConnectInstallation>(
            HttpMethod.Post, $"/v1/installations/{Uri.EscapeDataString(installationId)}/confirm", accessToken, key, null,
            cancellationToken);

    private async Task<T> SendAsync<T>(
        HttpMethod method, string path, string accessToken, ConnectDeviceKey key, object? body, CancellationToken cancellationToken)
    {
        var url = new Uri(connectOrigin, path);
        using var request = new HttpRequestMessage(method, url);
        request.Headers.Authorization = new AuthenticationHeaderValue("Bearer", accessToken);
        request.Headers.Add("DPoP", key.CreateProof(method.Method, url));
        if (body is not null)
        {
            request.Content = new StringContent(JsonSerializer.Serialize(body), Encoding.UTF8, "application/json");
        }

        using var response = await http.SendAsync(request, cancellationToken);
        var text = await response.Content.ReadAsStringAsync(cancellationToken);
        if (!response.IsSuccessStatusCode)
        {
            throw Denial(response.StatusCode, text);
        }

        return JsonSerializer.Deserialize<T>(text)
            ?? throw new InvalidOperationException("kombify Connect returned an empty response.");
    }

    private static ConnectDenialException Denial(HttpStatusCode status, string text)
    {
        try
        {
            using var document = JsonDocument.Parse(text);
            var root = document.RootElement;
            var guidance = root.GetProperty("user_guidance");
            return new ConnectDenialException(
                status,
                root.GetProperty("reason_code").GetString() ?? "connect_unknown",
                guidance.GetProperty("title").GetString() ?? "Connect request refused",
                guidance.GetProperty("body").GetString() ?? "");
        }
        catch (Exception ex) when (ex is JsonException or KeyNotFoundException or InvalidOperationException)
        {
            return new ConnectDenialException(status, "connect_unknown", "Connect request refused",
                $"kombify Connect answered HTTP {(int)status}.");
        }
    }

    private sealed record InstallationList
    {
        [JsonPropertyName("installations")] public List<ConnectInstallation> Installations { get; init; } = [];
    }

    private sealed record ChallengeResponse
    {
        [JsonPropertyName("challenge")] public string Challenge { get; init; } = "";
    }
}

/// <summary>
/// Auth0 refused the refresh grant for good: the session is gone. Every OAuth error on 400, 401 or 403 that is
/// neither transient nor an operator fault (<c>invalid_grant</c>, <c>expired_token</c>, <c>access_denied</c>,
/// <c>mfa_required</c>, <c>invalid_request</c>, unknown codes). The only refresh failure that ends a session.
/// </summary>
public sealed class Auth0GrantRefusedException(string error)
    : InvalidOperationException($"Auth0 refused the refresh token ({error}); sign in again.")
{
    public string Error { get; } = error;
}

/// <summary>
/// The refresh could not be completed for now: Auth0 answered 429 or 5xx, did not answer, or answered without an
/// OAuth error body (an HTML 403, a proxy 401), or with a transient code (<c>authorization_pending</c>,
/// <c>slow_down</c>, <c>too_many_requests</c>). The stored refresh token is still good and is kept.
/// </summary>
public class Auth0RefreshUnavailableException : InvalidOperationException
{
    /// <param name="possiblyRetired">
    /// True when the send itself may have retired the refresh token (a timeout, a connection lost after the request
    /// was written, or an answer that cannot be read): nothing more may be sent in this run.
    /// </param>
    public Auth0RefreshUnavailableException(string detail, TimeSpan? retryAfter, bool retryable, Exception? inner = null,
        bool timedOut = false, bool possiblyRetired = false)
        : this(FormatMessage(detail, CapRetryAfter(retryAfter)), inner, CapRetryAfter(retryAfter), retryable, timedOut,
            possiblyRetired)
    {
    }

    protected Auth0RefreshUnavailableException(string message, Exception? inner, TimeSpan? retryAfter, bool retryable,
        bool timedOut, bool possiblyRetired)
        : base(message, inner)
    {
        RetryAfter = CapRetryAfter(retryAfter);
        Retryable = retryable;
        TimedOut = timedOut;
        PossiblyRetired = possiblyRetired;
    }

    /// <summary>The <c>Retry-After</c> Auth0 sent, at most one hour.</summary>
    public TimeSpan? RetryAfter { get; }
    /// <summary>False for an answer that waiting in this process does not change.</summary>
    public bool Retryable { get; }
    /// <summary>The request timed out after it was sent (Auth0 may have received it).</summary>
    public bool TimedOut { get; }
    /// <summary>A timed-out send may have retired the refresh token; send nothing more in this run.</summary>
    public bool PossiblyRetired { get; }

    public static Auth0RefreshUnavailableException NotReadyForTimedOutToken(Exception? inner = null) =>
        new("kombify Connect is not ready; your sign-in is kept.", inner, retryAfter: null, retryable: false,
            timedOut: true, possiblyRetired: true);

    /// <summary>
    /// A send that may have retired the stored refresh token ended this run's renewals: nothing more is sent and "not
    /// ready" is reported. <paramref name="detail"/> names the cause the person can understand.
    /// </summary>
    public static Auth0RefreshUnavailableException NotReadyForPossiblyRetiredToken(string detail, Exception? inner = null) =>
        new($"kombify Connect is not ready ({detail}); your sign-in is kept.", inner, retryAfter: null, retryable: false,
            timedOut: false, possiblyRetired: true);

    private static TimeSpan? CapRetryAfter(TimeSpan? retryAfter) =>
        retryAfter is { } wait && wait > TimeSpan.Zero
            ? TimeSpan.FromTicks(Math.Min(wait.Ticks, TimeSpan.FromHours(1).Ticks))
            : null;

    private static string FormatMessage(string detail, TimeSpan? retryAfter)
    {
        if (retryAfter is null) return $"{detail}; your sign-in is kept, try again in a moment.";
        var totalSeconds = Math.Max(1, (int)Math.Ceiling(retryAfter.Value.TotalSeconds));
        string wait;
        if (totalSeconds >= 3600) wait = "1 hour";
        else if (totalSeconds >= 60)
        {
            var minutes = (int)Math.Ceiling(totalSeconds / 60.0);
            wait = minutes >= 60 ? "1 hour" : $"{minutes} min";
        }
        else
        {
            wait = $"{totalSeconds} s";
        }
        return $"{detail}; your sign-in is kept, retry in {wait}.";
    }
}

/// <summary>
/// Auth0 answered <c>invalid_client</c> or <c>unauthorized_client</c>: the registration is wrong, not the person's
/// session. The stored refresh token is kept (the client heals once the registration is repaired), the program is
/// reported as not ready, and no sign-in is started.
/// </summary>
public sealed class Auth0ClientNotReadyException(string error, TimeSpan? retryAfter)
    : Auth0RefreshUnavailableException($"kombify Connect is not ready (Auth0 answered {error}); your sign-in is kept.",
        inner: null, retryAfter, retryable: false, timedOut: false, possiblyRetired: false)
{
    public string Error { get; } = error;
}

/// <summary>
/// The caller stopped a refresh send in flight. Auth0 may have received the grant and retired the token, so nothing
/// more is sent in this run; the exception stays an <see cref="OperationCanceledException"/> so a closed window is
/// still a cancelled action, not a failure dialog.
/// </summary>
public sealed class ConnectRefreshCancelledException : OperationCanceledException
{
    public ConnectRefreshCancelledException(Exception? inner, CancellationToken cancellationToken)
        : base("The Connect refresh was cancelled after the request was sent; its token may be retired.", inner,
            cancellationToken)
    {
    }

    /// <summary>The cancelled send may have retired the refresh token: send nothing more in this run.</summary>
    public bool PossiblyRetired => true;
}

/// <summary>Result of the Auth0 native login: the access token stays in memory only.</summary>
public sealed record Auth0NativeSession(string AccessToken, string? RefreshToken, string SubjectId);

/// <summary>
/// Auth0 authorization code + PKCE with a loopback redirect (RFC 8252). Login, signup and
/// account choice stay in Auth0's Universal Login in the system browser.
/// </summary>
public sealed class Auth0NativeLogin(HttpClient http, Uri issuer, string clientId, string audience, IReadOnlyList<int> loopbackPorts)
{
    public async Task<Auth0NativeSession> SignInAsync(Action<Uri> openBrowser, CancellationToken cancellationToken)
    {
        var verifier = ConnectDeviceKey.Base64Url(RandomNumberGenerator.GetBytes(32));
        var codeChallenge = ConnectDeviceKey.Base64Url(SHA256.HashData(Encoding.ASCII.GetBytes(verifier)));
        var state = ConnectDeviceKey.Base64Url(RandomNumberGenerator.GetBytes(16));
        using var listener = StartListener(out var redirectUri);

        var authorize = new UriBuilder(new Uri(issuer, "/authorize"))
        {
            Query = string.Join("&", new Dictionary<string, string>
            {
                ["response_type"] = "code",
                ["client_id"] = clientId,
                ["redirect_uri"] = redirectUri,
                ["audience"] = audience,
                ["scope"] = "openid profile offline_access",
                ["code_challenge"] = codeChallenge,
                ["code_challenge_method"] = "S256",
                ["state"] = state,
            }.Select(pair => $"{pair.Key}={Uri.EscapeDataString(pair.Value)}")),
        }.Uri;
        openBrowser(authorize);

        var context = await listener.GetContextAsync().WaitAsync(TimeSpan.FromMinutes(10), cancellationToken);
        var query = context.Request.QueryString;
        var ok = query["state"] == state && !string.IsNullOrEmpty(query["code"]);
        var page = Encoding.UTF8.GetBytes(ok
            ? "<html><body>kombify sign-in finished. You can close this tab.</body></html>"
            : "<html><body>kombify sign-in failed. Return to the app and try again.</body></html>");
        context.Response.ContentType = "text/html; charset=utf-8";
        await context.Response.OutputStream.WriteAsync(page, cancellationToken);
        context.Response.Close();
        if (!ok)
        {
            throw new InvalidOperationException(query["error_description"] ?? "Auth0 sign-in did not return a valid code.");
        }

        using var exchange = await http.PostAsync(new Uri(issuer, "/oauth/token"), new FormUrlEncodedContent(new Dictionary<string, string>
        {
            ["grant_type"] = "authorization_code",
            ["client_id"] = clientId,
            ["code"] = query["code"]!,
            ["code_verifier"] = verifier,
            ["redirect_uri"] = redirectUri,
        }), cancellationToken);
        var text = await exchange.Content.ReadAsStringAsync(cancellationToken);
        if (!exchange.IsSuccessStatusCode)
        {
            throw new InvalidOperationException($"Auth0 token exchange failed with HTTP {(int)exchange.StatusCode}.");
        }

        using var document = JsonDocument.Parse(text);
        var accessToken = document.RootElement.GetProperty("access_token").GetString()!;
        var refreshToken = document.RootElement.TryGetProperty("refresh_token", out var refresh) ? refresh.GetString() : null;
        return new Auth0NativeSession(accessToken, refreshToken, SubjectOf(accessToken));
    }

    private static readonly HashSet<string> TransientErrors = new(StringComparer.Ordinal)
    {
        "authorization_pending", "slow_down", "too_many_requests",
    };

    private static readonly HashSet<string> OperatorErrors = new(StringComparer.Ordinal)
    {
        "invalid_client", "unauthorized_client",
    };

    private const int RefreshAttempts = 4;
    private static readonly TimeSpan RefreshRequestTimeout = TimeSpan.FromSeconds(15);
    private static readonly TimeSpan RefreshFirstPause = TimeSpan.FromSeconds(1);
    private static readonly TimeSpan RefreshBudget = TimeSpan.FromSeconds(25);

    /// <summary>
    /// Exchanges a stored refresh token once; Auth0 rotation may return a new one. Three classes:
    /// transient (429, 5xx, network fault, timeout, an answer without an OAuth error body, a transient code)
    /// throws <see cref="Auth0RefreshUnavailableException"/>; an operator fault (<c>invalid_client</c>,
    /// <c>unauthorized_client</c>) throws <see cref="Auth0ClientNotReadyException"/>; every other OAuth error on
    /// 400, 401 or 403 throws <see cref="Auth0GrantRefusedException"/> (session gone). The refresh token is still
    /// valid after the first two.
    /// </summary>
    public Task<Auth0NativeSession> RefreshAsync(string refreshToken, CancellationToken cancellationToken) =>
        RefreshCoreAsync(refreshToken, RefreshRequestTimeout, cancellationToken);

    private async Task<Auth0NativeSession> RefreshCoreAsync(
        string refreshToken, TimeSpan requestTimeout, CancellationToken cancellationToken)
    {
        HttpStatusCode status;
        TimeSpan? retryAfter;
        string text;
        using var timeoutCts = CancellationTokenSource.CreateLinkedTokenSource(cancellationToken);
        timeoutCts.CancelAfter(requestTimeout);
        try
        {
            using var exchange = await http.PostAsync(new Uri(issuer, "/oauth/token"), new FormUrlEncodedContent(new Dictionary<string, string>
            {
                ["grant_type"] = "refresh_token",
                ["client_id"] = clientId,
                ["refresh_token"] = refreshToken,
            }), timeoutCts.Token);
            status = exchange.StatusCode;
            retryAfter = RetryAfterOf(exchange.Headers.RetryAfter);
            text = await exchange.Content.ReadAsStringAsync(timeoutCts.Token);
        }
        catch (OperationCanceledException ex) when (cancellationToken.IsCancellationRequested)
        {
            // The caller stopped the send in flight: Auth0 may have received the grant and retired the token.
            throw new ConnectRefreshCancelledException(ex, cancellationToken);
        }
        catch (Exception ex) when (ex is HttpRequestException or IOException
            || (ex is OperationCanceledException && !cancellationToken.IsCancellationRequested))
        {
            // A fault of the network or a timeout of the request, not of the caller. A fault that happened after the
            // request was written may have retired the token; a name, connect or TLS fault never reaches Auth0.
            throw new Auth0RefreshUnavailableException("Auth0 could not be reached", null, retryable: true, ex,
                timedOut: ex is OperationCanceledException, possiblyRetired: MayHaveReachedAuth0(ex));
        }

        if (status is >= HttpStatusCode.OK and < HttpStatusCode.MultipleChoices)
        {
            try
            {
                using var document = JsonDocument.Parse(text);
                var accessToken = document.RootElement.GetProperty("access_token").GetString();
                if (string.IsNullOrEmpty(accessToken)) throw new InvalidOperationException("no access token");
                var rotated = document.RootElement.TryGetProperty("refresh_token", out var refresh) ? refresh.GetString() : null;
                return new Auth0NativeSession(accessToken, rotated ?? refreshToken, SubjectOf(accessToken));
            }
            catch (Exception ex) when (ex is JsonException or KeyNotFoundException or InvalidOperationException or FormatException)
            {
                // A 200 whose body carries no usable token: Auth0 may have rotated and retired the sent token already.
                throw Auth0RefreshUnavailableException.NotReadyForPossiblyRetiredToken(
                    "Auth0 answered the refresh without a usable token", ex);
            }
        }

        if (status is HttpStatusCode.BadRequest or HttpStatusCode.Unauthorized or HttpStatusCode.Forbidden
            && ErrorCodeOf(text) is { Length: > 0 } error && !TransientErrors.Contains(error))
        {
            if (OperatorErrors.Contains(error)) throw new Auth0ClientNotReadyException(error, retryAfter);
            throw new Auth0GrantRefusedException(error);
        }

        throw new Auth0RefreshUnavailableException($"Auth0 renewal failed with HTTP {(int)status}", retryAfter, retryable: true);
    }

    /// <summary>
    /// <see cref="RefreshAsync"/> with backoff inside Auth0's 30 s reuse leeway: each exchange is capped at 15 s
    /// inside a 25 s renewal budget. A non-timeout transient failure is tried again (1 s, 2 s, 4 s; the longer of
    /// that and <c>Retry-After</c>, up to an hour) within the 25 s budget; the last allowed attempt ends the loop, so
    /// the token is sent at most <see cref="RefreshAttempts"/> times. A timeout after the request was sent is retried
    /// at most once immediately within the budget; if a timed-out send may have retired the token and that retry
    /// fails, nothing more is sent in this run and "not ready" is reported. A refused grant and an operator fault are
    /// thrown at once, without a retry. A failure that may have retired the token (a timeout, a connection lost after
    /// the request was written, an answer without a usable token) carries <see cref="Auth0RefreshUnavailableException.PossiblyRetired"/>,
    /// so the run sends nothing more for that credential.
    /// </summary>
    public async Task<Auth0NativeSession> RefreshWithBackoffAsync(
        string refreshToken, CancellationToken cancellationToken, Func<TimeSpan, CancellationToken, Task>? delay = null)
    {
        delay ??= (wait, token) => Task.Delay(wait, token);
        var elapsed = Stopwatch.StartNew();
        var pause = RefreshFirstPause;
        var timedOut = false;          // a send timed out: one immediate retry, then nothing more
        var possiblyRetired = false;   // a send may have retired the token: later calls in this run send nothing
        for (var attempt = 1; ; attempt++)
        {
            var remaining = RefreshBudget - elapsed.Elapsed;
            if (remaining <= TimeSpan.Zero)
            {
                throw possiblyRetired
                    ? Auth0RefreshUnavailableException.NotReadyForTimedOutToken()
                    : new Auth0RefreshUnavailableException("Auth0 could not be reached", null, retryable: false, timedOut: true);
            }
            var attemptTimeout = remaining < RefreshRequestTimeout ? remaining : RefreshRequestTimeout;
            try
            {
                return await RefreshCoreAsync(refreshToken, attemptTimeout, cancellationToken);
            }
            catch (Auth0RefreshUnavailableException ex) when (ex.TimedOut && !timedOut && elapsed.Elapsed < RefreshBudget)
            {
                timedOut = true;
                possiblyRetired = true;
            }
            catch (Auth0RefreshUnavailableException ex) when ((ex.TimedOut || timedOut) && ex is not Auth0ClientNotReadyException)
            {
                // A timed-out send may have retired the token: send nothing more and report not ready.
                throw Auth0RefreshUnavailableException.NotReadyForTimedOutToken(ex);
            }
            catch (Auth0RefreshUnavailableException ex) when (ex.Retryable)
            {
                possiblyRetired |= ex.PossiblyRetired;
                var wait = ex.RetryAfter is { } retryAfter && retryAfter > pause ? retryAfter : pause;
                // The last allowed attempt, and one whose wait no longer fits the budget, end the loop: no further
                // send happens in this run.
                if (attempt >= RefreshAttempts || elapsed.Elapsed + wait > RefreshBudget)
                {
                    if (!possiblyRetired) throw;
                    throw Auth0RefreshUnavailableException.NotReadyForPossiblyRetiredToken("the last renewal did not finish", ex);
                }
                await delay(wait, cancellationToken);
                pause *= 2;
            }
        }
    }

    /// <summary>
    /// True when a transport fault may have reached Auth0: the connection was established, so the refresh token may
    /// have been received and retired. A name-resolution, connect, TLS or proxy-tunnel fault never gets that far.
    /// </summary>
    private static bool MayHaveReachedAuth0(Exception ex) => ex switch
    {
        HttpRequestException http => http.HttpRequestError is not (
            HttpRequestError.NameResolutionError or HttpRequestError.ConnectionError
            or HttpRequestError.SecureConnectionError or HttpRequestError.ProxyTunnelError
            or HttpRequestError.VersionNegotiationError or HttpRequestError.ExtendedConnectNotSupported),
        // The answer could not be read after the request was written.
        IOException => true,
        // A request timeout after the send started.
        OperationCanceledException => true,
        _ => false,
    };

    private static TimeSpan? RetryAfterOf(RetryConditionHeaderValue? header)
    {
        if (header is null) return null;
        if (header.Delta is { } delta) return delta > TimeSpan.Zero ? delta : null;
        if (header.Date is { } date)
        {
            var wait = date - DateTimeOffset.UtcNow;
            return wait > TimeSpan.Zero ? wait : null;
        }
        return null;
    }

    private static string? ErrorCodeOf(string text)
    {
        try
        {
            using var document = JsonDocument.Parse(text);
            return document.RootElement.ValueKind == JsonValueKind.Object
                && document.RootElement.TryGetProperty("error", out var error) && error.ValueKind == JsonValueKind.String
                ? error.GetString()
                : null;
        }
        catch (JsonException)
        {
            return null;
        }
    }

    private HttpListener StartListener(out string redirectUri)
    {
        foreach (var port in loopbackPorts)
        {
            var listener = new HttpListener();
            var prefix = $"http://127.0.0.1:{port}/oauth/callback/";
            listener.Prefixes.Add(prefix);
            try
            {
                listener.Start();
                redirectUri = prefix.TrimEnd('/');
                return listener;
            }
            catch (HttpListenerException)
            {
                listener.Close();
            }
        }

        throw new InvalidOperationException("No loopback port for the kombify sign-in callback is free.");
    }

    /// <summary>Reads <c>sub</c> for the device_ref only; the Gateway validates the token itself.</summary>
    private static string SubjectOf(string jwt)
    {
        var parts = jwt.Split('.');
        if (parts.Length < 2)
        {
            return "";
        }

        var payload = parts[1].Replace('-', '+').Replace('_', '/');
        payload = payload.PadRight(payload.Length + (4 - payload.Length % 4) % 4, '=');
        using var document = JsonDocument.Parse(Convert.FromBase64String(payload));
        return document.RootElement.TryGetProperty("sub", out var sub) ? sub.GetString() ?? "" : "";
    }
}
