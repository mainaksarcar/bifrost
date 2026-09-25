import { describe, expect, it } from "vitest";
import { DefaultNetworkConfig } from "@/lib/constants/config";
import { ProviderFormSchema } from "./providerForm";
import { modelProviderKeySchema } from "@/lib/types/schemas";

// The provider form validates through ProviderFormSchema, and zodResolver hands
// react-hook-form the parsed result. Zod strips undeclared keys, so a credential the schema
// does not know about never reaches the API. That failure is silent: the form saves, reports
// success, and the key arrives with no GitHub App credentials at all.
describe("ProviderFormSchema github-copilot credentials", () => {
	const literal = (value: string) => ({ value, ref: "" });
	const envRef = (ref: string) => ({ value: "", ref, type: "env" as const });
	const validPEM = "-----BEGIN RSA PRIVATE KEY-----\nMIIBOgIBAAJBAKj3\n-----END RSA PRIVATE KEY-----";

	const appConfig = (overrides: Record<string, unknown> = {}) => ({
		app_id: literal("123456"),
		installation_id: literal("87654321"),
		repository_id: literal("999000111"),
		private_key: literal(validPEM),
		...overrides,
	});

	const form = (keyOverrides: Record<string, unknown>) => ({
		selectedProvider: "github-copilot",
		isDirty: true,
		networkConfig: DefaultNetworkConfig,
		keys: [{ id: "k1", name: "copilot", value: "", models: ["*"], weight: 1, ...keyOverrides }],
	});

	const parse = (keyOverrides: Record<string, unknown>) => ProviderFormSchema.safeParse(form(keyOverrides));

	it("keeps github_copilot_key_config through parsing", () => {
		const parsed = parse({ github_copilot_key_config: appConfig() });

		expect(parsed.success, parsed.success ? "" : JSON.stringify(parsed.error.issues)).toBe(true);
		if (!parsed.success) return;

		const key = parsed.data.keys[0];
		expect(key.github_copilot_key_config, "credentials were stripped, so the key would save with no auth").toBeDefined();
		expect(key.github_copilot_key_config?.app_id?.value).toBe("123456");
		expect(key.github_copilot_key_config?.private_key?.value).toContain("BEGIN RSA PRIVATE KEY");
	});

	it("accepts a direct Copilot token with no app config", () => {
		expect(parse({ value: "tid=abc" }).success).toBe(true);
	});

	it("rejects a key with neither a token nor app credentials", () => {
		expect(parse({}).success, "a key with no credential at all must not save").toBe(false);
	});

	it("rejects an app config that only carries github_domain", () => {
		// The nested schema permits an empty block so token auth stays valid, which means
		// the outer check is the only thing standing between this and a credential-less key.
		expect(parse({ github_copilot_key_config: { github_domain: literal("acme.ghe.com") } }).success).toBe(false);
	});

	it("rejects a partially filled app config", () => {
		expect(parse({ github_copilot_key_config: { app_id: literal("123456") } }).success).toBe(false);
	});

	describe("literal credential formats", () => {
		it("rejects a non-numeric installation_id", () => {
			expect(parse({ github_copilot_key_config: appConfig({ installation_id: literal("my-install") }) }).success).toBe(false);
		});

		it("rejects a path-traversal installation_id", () => {
			expect(parse({ github_copilot_key_config: appConfig({ installation_id: literal("1/../../../user") }) }).success).toBe(false);
		});

		it("rejects a non-numeric repository_id", () => {
			expect(parse({ github_copilot_key_config: appConfig({ repository_id: literal("my-repo") }) }).success).toBe(false);
		});

		it("rejects a private key that is not PEM", () => {
			expect(parse({ github_copilot_key_config: appConfig({ private_key: literal("not a key") }) }).success).toBe(false);
		});

		it("accepts a PKCS#8 private key", () => {
			const pkcs8 = "-----BEGIN PRIVATE KEY-----\nMIIBOgIBAAJBAKj3\n-----END PRIVATE KEY-----";
			expect(parse({ github_copilot_key_config: appConfig({ private_key: literal(pkcs8) }) }).success).toBe(true);
		});

		it("rejects a literal disguised by a type tag with no ref", () => {
			// type: "env" with an empty ref is still a literal - the value is right there.
			// Trusting the tag alone lets any malformed value skip every format check.
			const disguised = { value: "not-numeric", ref: "", type: "env" as const };
			expect(parse({ github_copilot_key_config: appConfig({ installation_id: disguised }) }).success).toBe(false);
		});

		it.each([
			["EC key", "-----BEGIN EC PRIVATE KEY-----\nMHcCAQE\n-----END EC PRIVATE KEY-----"],
			["encrypted key", "-----BEGIN ENCRYPTED PRIVATE KEY-----\nMIIBu\n-----END ENCRYPTED PRIVATE KEY-----"],
			["mismatched headers", "-----BEGIN RSA PRIVATE KEY-----\nMIIBu\n-----END PRIVATE KEY-----"],
			["empty body", "-----BEGIN RSA PRIVATE KEY-----\n\n-----END RSA PRIVATE KEY-----"],
			["header only", "-----BEGIN RSA PRIVATE KEY-----"],
		])("rejects a private key that is a %s", (_name, body) => {
			expect(parse({ github_copilot_key_config: appConfig({ private_key: literal(body) }) }).success).toBe(false);
		});

		it("does not format-check environment references", () => {
			// Their values resolve on the server, so judging their shape here would reject
			// every legitimate headless configuration.
			const parsed = parse({
				github_copilot_key_config: {
					app_id: envRef("COPILOT_APP_ID"),
					installation_id: envRef("COPILOT_INSTALLATION_ID"),
					repository_id: envRef("COPILOT_REPOSITORY_ID"),
					private_key: envRef("COPILOT_PRIVATE_KEY"),
				},
			});
			expect(parsed.success, parsed.success ? "" : JSON.stringify(parsed.error.issues)).toBe(true);
		});
	});
});
// A brand-new Copilot key is seeded with auth_mode "oauth" so the device-login tab is
// the default. That seed must not make the untouched form report errors before the
// operator has typed anything.
describe("modelProviderKeySchema github-copilot OAuth client ID", () => {
	const key = (overrides: Record<string, unknown>) => ({
		id: "k1",
		name: "GitHub Copilot",
		models: ["*"],
		weight: 1,
		...overrides,
	});
	const clientIdIssue = (result: ReturnType<typeof modelProviderKeySchema.safeParse>) =>
		result.success ? [] : result.error.issues.filter((issue) => issue.path.join(".") === "github_copilot_key_config.auth_client_id");

	it("does not flag an untouched new key that has no credential yet", () => {
		const result = modelProviderKeySchema.safeParse(key({ github_copilot_key_config: { auth_mode: "oauth", auth_client_id: "" } }));

		expect(clientIdIssue(result), "a pristine Add-new-key form must not show a required error").toEqual([]);
	});

	it("flags a saved OAuth credential that carries no client ID", () => {
		const result = modelProviderKeySchema.safeParse(
			key({
				value: { value: "gho_token", ref: "" },
				github_copilot_key_config: { auth_mode: "oauth", auth_client_id: "" },
			}),
		);

		expect(clientIdIssue(result).map((issue) => issue.message)).toEqual(["OAuth Client ID is required"]);
	});

	it("accepts an OAuth credential with a client ID", () => {
		const result = modelProviderKeySchema.safeParse(
			key({
				value: { value: "gho_token", ref: "" },
				github_copilot_key_config: { auth_mode: "oauth", auth_client_id: "Ov23liExample" },
			}),
		);

		expect(clientIdIssue(result)).toEqual([]);
	});

	it("carries the refresh token and expiry a device login returns", () => {
		const result = modelProviderKeySchema.safeParse(
			key({
				value: { value: "ghu_token", ref: "" },
				github_copilot_key_config: {
					auth_mode: "oauth",
					auth_client_id: "Iv23liExample",
					refresh_token: { value: "ghr_token", ref: "" },
					token_expires_at: 1789543415,
				},
			}),
		);

		expect(result.success).toBe(true);
		if (result.success) {
			expect(result.data.github_copilot_key_config?.refresh_token?.value).toBe("ghr_token");
			expect(result.data.github_copilot_key_config?.token_expires_at).toBe(1789543415);
		}
	});

	// GitHub omits both for apps whose user tokens never expire, so a key without them is
	// ordinary rather than half-configured.
	it("accepts an OAuth credential with no refresh token", () => {
		const result = modelProviderKeySchema.safeParse(
			key({
				value: { value: "gho_token", ref: "" },
				github_copilot_key_config: { auth_mode: "oauth", auth_client_id: "Ov23liExample" },
			}),
		);

		expect(result.success).toBe(true);
	});

	// The refresh token arrives with the user token, so it must not read as a GitHub App
	// credential and trip the "do not combine" rule.
	it("does not treat a refresh token as an App credential", () => {
		const result = modelProviderKeySchema.safeParse(
			key({
				value: { value: "ghu_token", ref: "" },
				github_copilot_key_config: {
					auth_mode: "oauth",
					auth_client_id: "Iv23liExample",
					refresh_token: { value: "ghr_token", ref: "" },
				},
			}),
		);

		const combined = (result.success ? [] : result.error.issues).filter((issue) => issue.message.includes("Do not combine"));
		expect(combined).toEqual([]);
	});
});