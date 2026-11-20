export type HomelabTestUserRole =
  "admin" | "superuser" | "developer" | "oneliner" | "remote" | "cloud";

export interface HomelabTestUser {
  email: string;
  password: string;
}

export interface HomelabTestUserReadiness {
  emailConfigured: boolean;
  passwordConfigured: boolean;
  missingSecrets: string[];
}

type EnvSource = Record<string, string | undefined>;

const LOOPBACK_HOSTS = new Set(["localhost", "127.0.0.1", "::1"]);

const LOOPBACK_TARGET_ENV_KEYS = [
  "PLAYWRIGHT_BASE_URL",
  "HOMELAB_API_URL",
  "API_URL",
  "HOMELAB_URL",
  "HOMELAB_APP_URL",
];

const LOCAL_BOOTSTRAP_FALLBACKS: Partial<
  Record<
    HomelabTestUserRole,
    {
      emailEnv: string;
      passwordEnv: string;
      defaultEmail: string;
      defaultPassword: string;
    }
  >
> = {
  admin: {
    emailEnv: "HOMELAB_ADMIN_EMAIL",
    passwordEnv: "HOMELAB_ADMIN_PASSWORD",
    defaultEmail: "admin@homelab.local",
    defaultPassword: "dev-admin-password-change-me",
  },
  superuser: {
    emailEnv: "HOMELAB_SUPERUSER_EMAIL",
    passwordEnv: "HOMELAB_SUPERUSER_PASSWORD",
    defaultEmail: "superuser@homelab.local",
    defaultPassword: "dev-superuser-password-change-me",
  },
  developer: {
    emailEnv: "HOMELAB_DEVELOPER_EMAIL",
    passwordEnv: "HOMELAB_DEVELOPER_PASSWORD",
    defaultEmail: "developer@homelab.local",
    defaultPassword: "dev-developer-password-change-me",
  },
};

const ROLE_SECRET_CANDIDATES: Record<
  HomelabTestUserRole,
  { email: string; password: string }[]
> = {
  admin: [
    {
      email: "TEST_PRO_USER_EMAIL",
      password: "TEST_PRO_USER_PASSWORD",
    },
    { email: "TEST_ADMIN_EMAIL", password: "TEST_ADMIN_PASSWORD" },
    {
      email: "HOMELAB_E2E_ADMIN_EMAIL",
      password: "HOMELAB_E2E_ADMIN_PASSWORD",
    },
    { email: "HOMELAB_ADMIN_EMAIL", password: "HOMELAB_ADMIN_PASSWORD" },
  ],
  superuser: [
    {
      email: "TEST_GLOBAL_ADMIN_EMAIL",
      password: "TEST_GLOBAL_ADMIN_PASSWORD",
    },
    { email: "TEST_ADMIN_EMAIL", password: "TEST_ADMIN_PASSWORD" },
    {
      email: "HOMELAB_E2E_SUPERUSER_EMAIL",
      password: "HOMELAB_E2E_SUPERUSER_PASSWORD",
    },
    {
      email: "HOMELAB_SUPERUSER_EMAIL",
      password: "HOMELAB_SUPERUSER_PASSWORD",
    },
  ],
  developer: [
    {
      email: "TEST_DEVELOPER_EMAIL",
      password: "TEST_DEVELOPER_PASSWORD",
    },
    {
      email: "TEST_PRO_USER_EMAIL",
      password: "TEST_PRO_USER_PASSWORD",
    },
    {
      email: "HOMELAB_E2E_DEVELOPER_EMAIL",
      password: "HOMELAB_E2E_DEVELOPER_PASSWORD",
    },
    {
      email: "HOMELAB_DEVELOPER_EMAIL",
      password: "HOMELAB_DEVELOPER_PASSWORD",
    },
  ],
  oneliner: [
    {
      email: "TEST_RUNTIME_E2E_USER_EMAIL",
      password: "TEST_RUNTIME_E2E_USER_PASSWORD",
    },
    {
      email: "HOMELAB_E2E_ONELINER_EMAIL",
      password: "HOMELAB_E2E_ONELINER_PASSWORD",
    },
  ],
  remote: [
    {
      email: "TEST_RUNTIME_E2E_USER_EMAIL",
      password: "TEST_RUNTIME_E2E_USER_PASSWORD",
    },
    {
      email: "HOMELAB_E2E_REMOTE_EMAIL",
      password: "HOMELAB_E2E_REMOTE_PASSWORD",
    },
  ],
  cloud: [
    {
      email: "TEST_RIL_PRO_USER_EMAIL",
      password: "TEST_RIL_PRO_USER_PASSWORD",
    },
    {
      email: "HOMELAB_E2E_CLOUD_EMAIL",
      password: "HOMELAB_E2E_CLOUD_PASSWORD",
    },
  ],
};

// The explicit Centron lifecycle, and a lane that names it with
// HOMELAB_RUNTIME_E2E_CLOUD_OWNER=all-you-need (the paid journeys: the RIL Pro
// slot holds its persistent IONOS Cloud Kit), use the canonical ephemeral managed
// owner. Other cloud duties retain their existing Pro fixture and fallback contract.
export function getHomelabTestUserSecretCandidates(
  role: HomelabTestUserRole,
  env: EnvSource = process.env,
): { email: string; password: string }[] {
  if (role === "cloud" && (env.HOMELAB_RUNTIME_E2E_PROVIDER_ID?.trim().toLowerCase() === "centron" ||
      env.HOMELAB_RUNTIME_E2E_CLOUD_OWNER?.trim() === "all-you-need")) {
    return [{
      email: "TEST_ALL_YOU_NEED_USER_EMAIL",
      password: "TEST_ALL_YOU_NEED_USER_PASSWORD",
    }];
  }
  return ROLE_SECRET_CANDIDATES[role];
}

function readSecret(env: EnvSource, name: string): string {
  return env[name]?.trim() ?? "";
}

function isLoopbackTarget(env: EnvSource): boolean {
  return LOOPBACK_TARGET_ENV_KEYS.some((key) => {
    const raw = env[key]?.trim();
    if (!raw) return false;

    try {
      return LOOPBACK_HOSTS.has(new URL(raw).hostname.toLowerCase());
    } catch {
      return false;
    }
  });
}

function getLocalBootstrapUser(
  role: HomelabTestUserRole,
  env: EnvSource,
): HomelabTestUser | null {
  if (!isLoopbackTarget(env)) {
    return null;
  }

  const fallback = LOCAL_BOOTSTRAP_FALLBACKS[role];
  if (!fallback) {
    return null;
  }
  return {
    email: readSecret(env, fallback.emailEnv) || fallback.defaultEmail,
    password: readSecret(env, fallback.passwordEnv) || fallback.defaultPassword,
  };
}

function describeCandidates(
  role: HomelabTestUserRole,
  field: "email" | "password",
  env: EnvSource,
): string {
  return getHomelabTestUserSecretCandidates(role, env)
    .map((candidate) => candidate[field])
    .join(" or ");
}

function candidateUsers(
  role: HomelabTestUserRole,
  env: EnvSource,
): HomelabTestUser[] {
  const users = getHomelabTestUserSecretCandidates(role, env).map((candidate) => ({
    email: readSecret(env, candidate.email),
    password: readSecret(env, candidate.password),
  }));
  // Explicit canonical custody must be complete. Never silently select a
  // different identity when only half of that pair reached the consumer.
  const [first] = users;
  if (role === "cloud" && first && (first.email || first.password)) {
    return users.slice(0, 1);
  }
  return users;
}

export function getHomelabTestUser(
  role: HomelabTestUserRole = "admin",
  env: EnvSource = process.env,
): HomelabTestUser {
  const configuredUser = candidateUsers(role, env).find(
    (candidate) => candidate.email && candidate.password,
  );
  if (configuredUser) return configuredUser;

  const localBootstrapUser = getLocalBootstrapUser(role, env);
  if (localBootstrapUser) {
    return localBootstrapUser;
  }

  return { email: "", password: "" };
}

export function buildHomelabTestUserReadiness(
  env: EnvSource = process.env,
): Record<HomelabTestUserRole, HomelabTestUserReadiness> {
  return Object.fromEntries(
    (Object.keys(ROLE_SECRET_CANDIDATES) as HomelabTestUserRole[]).map(
      (role) => {
        const localBootstrapUser = getLocalBootstrapUser(role, env);
        const users = candidateUsers(role, env);
        const configuredUser = users.find(
          (candidate) => candidate.email && candidate.password,
        );
        const selectedUser =
          configuredUser ??
          users.find((candidate) => candidate.email || candidate.password);
        const emailConfigured =
          !!localBootstrapUser || Boolean(selectedUser?.email);
        const passwordConfigured =
          !!localBootstrapUser || Boolean(selectedUser?.password);

        return [
          role,
          {
            emailConfigured,
            passwordConfigured,
            missingSecrets: [
              emailConfigured ? "" : describeCandidates(role, "email", env),
              passwordConfigured ? "" : describeCandidates(role, "password", env),
            ].filter(Boolean),
          },
        ];
      },
    ),
  ) as Record<HomelabTestUserRole, HomelabTestUserReadiness>;
}

export function requireHomelabTestUser(
  role: HomelabTestUserRole = "admin",
  env: EnvSource = process.env,
): HomelabTestUser {
  const user = getHomelabTestUser(role, env);
  if (user.email && user.password) return user;

  const readiness = buildHomelabTestUserReadiness(env)[role];
  throw new Error(
    [
      `Missing required Homelab Runtime E2E test-user secrets for ${role}.`,
      `Configure ${readiness.missingSecrets.join(", ")} via the configured environment before running happy-path tests.`,
      "401/403 responses are not acceptable release-test substitutes.",
    ].join(" "),
  );
}
