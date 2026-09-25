import { Alert, AlertDescription, AlertTitle } from "@/components/ui/alert";
import { Button } from "@/components/ui/button";
import { FormControl, FormDescription, FormField, FormItem, FormLabel, FormMessage } from "@/components/ui/form";
import { Input } from "@/components/ui/input";
import { SecretVarInput } from "@/components/ui/secretVarInput";
import { Separator } from "@/components/ui/separator";
import { Tabs, TabsList, TabsTrigger } from "@/components/ui/tabs";
import { modelProviderKeySchema } from "@/lib/types/schemas";
import { getApiBaseUrl } from "@/lib/utils/port";
import { hasCopilotApiToken } from "@/lib/utils/validation";
import { CheckCircle2, Copy, ExternalLink, Info, Loader2 } from "lucide-react";
import { useCallback, useEffect, useRef, useState } from "react";
import { UseFormReturn } from "react-hook-form";
import { z } from "zod";

type Values = { key: z.infer<typeof modelProviderKeySchema> };
type AuthMode = "oauth" | "api_token";
type ModeCredential = { value: Values["key"]["value"]; config: Values["key"]["github_copilot_key_config"] };
type Flow = { flowId: string; userCode: string; verificationUri: string; interval: number; expires: number };
type Status = "idle" | "awaiting" | "complete" | "error";

// The handler only ever issues this verification URI; anything else is a tampered response.
const GITHUB_DEVICE_URI = "https://github.com/login/device";

// What a completed device login yields. Only the access token is guaranteed: GitHub omits
// the other two for apps whose user tokens do not expire, and those keys never renew.
export type CopilotCredential = {
	accessToken: string;
	refreshToken?: string;
	expiresAt?: number;
};

function AuthStatusCard({ reauthorizing, onToggle, disabled }: { reauthorizing: boolean; onToggle: () => void; disabled: boolean }) {
	return (
		<div className="bg-muted/40 rounded-sm border p-4" data-testid="copilot-auth-status-card">
			<div className="flex items-start justify-between gap-4">
				<div className="flex items-start gap-3">
					<CheckCircle2 className="text-primary mt-0.5 size-5 shrink-0" />
					<div>
						<p className="text-sm font-medium">Authenticated with GitHub</p>
						<p className="text-muted-foreground mt-0.5 text-xs">
							Token saved.{" "}
							{reauthorizing ? "Complete the flow below to replace it." : "Re-authenticate to swap the token without downtime."}
						</p>
					</div>
				</div>
				<Button type="button" variant="outline" size="sm" onClick={onToggle} disabled={disabled} data-testid="copilot-reauth-toggle">
					{reauthorizing ? "Cancel" : "Re-authenticate"}
				</Button>
			</div>
		</div>
	);
}

export function CopilotAuthForm({
	form,
	onAuthorized,
	disabled,
}: {
	form: UseFormReturn<Values>;
	onAuthorized: (credential: CopilotCredential) => Promise<void>;
	disabled: boolean;
}) {
	const mode = form.watch("key.github_copilot_key_config.auth_mode") ?? "api_token";
	const clientID = form.watch("key.github_copilot_key_config.auth_client_id") ?? "";
	const authenticated = hasCopilotApiToken(form.watch("key.value"));
	const [flow, setFlow] = useState<Flow>();
	const [status, setStatus] = useState<Status>("idle");
	const [countdown, setCountdown] = useState<number | null>(null);
	const [checking, setChecking] = useState(false);
	const [copied, setCopied] = useState(false);
	const [reauthorizing, setReauthorizing] = useState(false);
	const [message, setMessage] = useState("");
	// The saved token belongs to this client ID, and editing it clears the token.
	// Re-authenticate is the deliberate way to do that, so nothing else is.
	const clientIDLocked = authenticated && !reauthorizing;
	const stashed = useRef<Partial<Record<AuthMode, ModeCredential>>>({});
	const preReauth = useRef<ModeCredential | undefined>(undefined);
	const generation = useRef(0);
	const authorizeRef = useRef(onAuthorized);
	authorizeRef.current = onAuthorized;

	const resetFlow = useCallback(() => {
		generation.current++;
		setFlow(undefined);
		setStatus("idle");
		setCountdown(null);
		setChecking(false);
		setCopied(false);
		setMessage("");
	}, []);

	// A different client ID is a different OAuth app, so any in-flight authorization is void.
	useEffect(() => {
		resetFlow();
		return () => {
			generation.current++;
		};
	}, [mode, clientID, resetFlow]);

	useEffect(() => {
		if (countdown === null || countdown <= 0) return;
		const timer = setTimeout(() => setCountdown((value) => (value === null ? null : value - 1)), 1000);
		return () => clearTimeout(timer);
	}, [countdown]);

	const poll = useCallback(async () => {
		if (!flow || mode !== "oauth") return;
		const current = generation.current;
		setCountdown(null);
		setChecking(true);
		try {
			if (Date.now() >= flow.expires) {
				setFlow(undefined);
				setStatus("error");
				setMessage("The device code expired. Start the authorization again.");
				return;
			}
			const response = await fetch(`${getApiBaseUrl()}/providers/github-copilot/device-login/poll`, {
				method: "POST",
				credentials: "include",
				headers: { "Content-Type": "application/json" },
				body: JSON.stringify({ flow_id: flow.flowId }),
			});
			if (!response.ok) throw new Error("poll failed");
			const result = await response.json();
			if (generation.current !== current) return;
			if (result.status === "complete" && typeof result.access_token === "string") {
				setFlow(undefined);
				try {
					// GitHub returns the refresh token and expiry only for apps whose tokens
					// expire; the others are saved without them and simply never renew.
					await authorizeRef.current({
						accessToken: result.access_token,
						refreshToken: typeof result.refresh_token === "string" ? result.refresh_token : undefined,
						expiresAt: typeof result.token_expires_at === "number" ? result.token_expires_at : undefined,
					});
					if (generation.current !== current) return;
					preReauth.current = undefined;
					setReauthorizing(false);
					setStatus("complete");
					setMessage("Token saved. Models were refreshed for this account.");
				} catch {
					if (generation.current !== current) return;
					setStatus("error");
					setMessage("Authorization succeeded, but the key was not saved. Fix the form errors and click Save.");
				}
				return;
			}
			if (result.status === "pending") {
				const interval = Math.max(5, Number(result.interval) || flow.interval);
				setFlow({ ...flow, interval });
				setCountdown(interval);
				return;
			}
			setFlow(undefined);
			setStatus("error");
			setMessage("Authorization was denied or expired. Start the authorization again.");
		} catch {
			if (generation.current === current) {
				setStatus("error");
				setMessage("Could not reach the server to check authorization. Try again.");
			}
		} finally {
			if (generation.current === current) setChecking(false);
		}
	}, [flow, mode]);

	useEffect(() => {
		if (countdown === 0 && !checking && flow && mode === "oauth") void poll();
	}, [countdown, checking, flow, mode, poll]);

	async function initiate() {
		const current = ++generation.current;
		setFlow(undefined);
		setStatus("awaiting");
		setMessage("");
		setChecking(true);
		try {
			const response = await fetch(`${getApiBaseUrl()}/providers/github-copilot/device-login/initiate`, {
				method: "POST",
				credentials: "include",
				headers: { "Content-Type": "application/json" },
				body: JSON.stringify({ client_id: clientID.trim() }),
			});
			if (!response.ok) throw new Error("initiate failed");
			const result = await response.json();
			if (generation.current !== current) return;
			if (result.verification_uri !== GITHUB_DEVICE_URI || typeof result.flow_id !== "string" || typeof result.user_code !== "string")
				throw new Error("invalid device response");
			const interval = Math.max(5, Number(result.interval) || 5);
			setFlow({
				flowId: result.flow_id,
				userCode: result.user_code,
				verificationUri: result.verification_uri,
				interval,
				expires: Date.now() + Number(result.expires_in) * 1000,
			});
			setCountdown(interval);
		} catch {
			if (generation.current === current) {
				setStatus("error");
				setMessage("Could not start device authorization. Check the Client ID and that device flow is enabled on the OAuth app.");
			}
		} finally {
			if (generation.current === current) setChecking(false);
		}
	}

	function copyCode() {
		if (!flow) return;
		navigator.clipboard
			.writeText(flow.userCode)
			.then(() => {
				setCopied(true);
				setTimeout(() => setCopied(false), 2000);
			})
			.catch(() => {
				setStatus("error");
				setMessage("Could not copy the device code. Copy it manually.");
			});
	}

	function toggleReauth() {
		if (reauthorizing) {
			// Editing the client ID clears the token minted for it, so backing out has to put
			// both back rather than leave the key half-erased.
			if (preReauth.current) {
				form.setValue("key.value", preReauth.current.value, { shouldDirty: true });
				form.setValue("key.github_copilot_key_config", preReauth.current.config, { shouldDirty: true });
				preReauth.current = undefined;
			}
			setReauthorizing(false);
			resetFlow();
			return;
		}
		preReauth.current = { value: form.getValues("key.value"), config: form.getValues("key.github_copilot_key_config") };
		setReauthorizing(true);
		setStatus("idle");
		setMessage("");
	}

	function selectMode(next: AuthMode) {
		if (next === mode) return;
		resetFlow();
		setReauthorizing(false);
		// Switching tabs is navigation, not a decision to throw away a credential that is
		// already saved, so each method holds on to its own until the user replaces it. An
		// abandoned re-authentication stashes what it started from, not its half-done draft.
		stashed.current[mode] = preReauth.current ?? {
			value: form.getValues("key.value"),
			config: form.getValues("key.github_copilot_key_config"),
		};
		preReauth.current = undefined;
		const restored = stashed.current[next];
		// Choosing a method is not a claim to have finished filling it in, so clearing the
		// field the user is about to type into must not report it as missing.
		form.setValue("key.value", restored?.value ?? { value: "", ref: "" }, { shouldDirty: true });
		form.setValue(
			"key.github_copilot_key_config",
			restored?.config ?? { auth_mode: next, ...(next === "oauth" ? { auth_client_id: clientID } : {}) },
			{ shouldDirty: true },
		);
	}

	return (
		<div className="space-y-4" data-testid="copilot-auth-form">
			<Separator className="my-6" />
			<div className="space-y-2">
				<FormLabel>Authentication Method</FormLabel>
				<Tabs value={mode} onValueChange={(next) => selectMode(next as "oauth" | "api_token")}>
					<TabsList className="grid w-full grid-cols-2">
						<TabsTrigger value="oauth" disabled={disabled} data-testid="apikey-copilot-device-login-tab">
							GitHub Device Login
						</TabsTrigger>
						<TabsTrigger value="api_token" disabled={disabled} data-testid="apikey-copilot-manual-token-tab">
							GitHub Copilot Token
						</TabsTrigger>
					</TabsList>
				</Tabs>
			</div>

			{mode === "oauth" ? (
				<div className="space-y-4">
					<FormField
						control={form.control}
						name="key.github_copilot_key_config.auth_client_id"
						render={({ field }) => (
							<FormItem>
								<FormLabel>Client ID</FormLabel>
								<FormControl>
									<Input
										{...field}
										value={field.value ?? ""}
										onChange={(event) => {
											resetFlow();
											field.onChange(event);
											// Editing the client ID invalidates any token minted for the old one, but
											// clearing it must not report the token field as missing before the user
											// has re-run the login.
											form.setValue("key.value", { value: "", ref: "" }, { shouldDirty: true });
										}}
										disabled={disabled || checking || clientIDLocked}
										autoComplete="off"
										placeholder="Iv23li... or Ov23li..."
										data-testid="copilot-client-id"
									/>
								</FormControl>
								<FormDescription>
									Create a GitHub App or OAuth App at{" "}
									<a
										href="https://github.com/settings/apps"
										target="_blank"
										rel="noopener noreferrer"
										className="underline underline-offset-2"
										data-testid="copilot-client-id-help-link"
									>
										github.com/settings/apps
									</a>
									, enable Device Flow, then install it on your account. Either type works; a GitHub App issues tokens that Bifrost renews
									for you, while an OAuth App issues ones that never expire.
								</FormDescription>
								<FormMessage />
							</FormItem>
						)}
					/>

					{/* Kept while re-authenticating even though editing the client ID clears the token:
					    the card carries Cancel, and losing it would strand the user mid-flow. */}
					{(authenticated || reauthorizing) && <AuthStatusCard reauthorizing={reauthorizing} onToggle={toggleReauth} disabled={disabled} />}

					{(!authenticated || reauthorizing) && (
						<>
							{!flow && (
								<>
									<Alert variant="default">
										<Info className="mt-0.5 h-4 w-4 flex-shrink-0 text-blue-600" />
										<AlertTitle>GitHub Copilot Authentication</AlertTitle>
										<AlertDescription>
											Sign in through the GitHub device flow using your own OAuth app. The account must have an active Copilot subscription,
											and the app must have device flow enabled.
										</AlertDescription>
									</Alert>
									<Button
										type="button"
										className="w-full"
										disabled={disabled || checking || !clientID.trim()}
										onClick={initiate}
										data-testid="copilot-device-login-button"
									>
										{checking ? (
											<>
												<Loader2 className="h-4 w-4 animate-spin" /> Starting...
											</>
										) : authenticated ? (
											"Re-authenticate with GitHub"
										) : (
											"Login with GitHub"
										)}
									</Button>
								</>
							)}

							{flow && (
								<div className="space-y-4">
									<Alert variant="default">
										<Info className="mt-0.5 h-4 w-4 flex-shrink-0 text-blue-600" />
										<AlertTitle>Enter this code on GitHub</AlertTitle>
										<AlertDescription className="space-y-3">
											<div className="flex items-center gap-3 pt-2">
												<code
													className="bg-muted rounded-md px-4 py-2 text-2xl font-bold tracking-widest"
													data-testid="copilot-device-code"
												>
													{flow.userCode}
												</code>
												<Button type="button" variant="outline" size="sm" onClick={copyCode} data-testid="copilot-copy-code-button">
													{copied ? <CheckCircle2 className="h-4 w-4 text-green-600" /> : <Copy className="h-4 w-4" />}
												</Button>
											</div>
											<p className="text-sm">
												Visit{" "}
												<a
													href={flow.verificationUri}
													target="_blank"
													rel="noopener noreferrer"
													className="inline-flex items-center gap-1 text-blue-600 underline hover:text-blue-700"
													data-testid="copilot-verification-link"
												>
													github.com/login/device
													<ExternalLink className="h-3 w-3" />
												</a>{" "}
												and enter the code to authorize.
											</p>
										</AlertDescription>
									</Alert>

									<Button
										type="button"
										className="w-full"
										disabled={disabled || checking}
										onClick={poll}
										data-testid="copilot-confirm-auth-button"
									>
										{checking ? (
											<>
												<Loader2 className="h-4 w-4 animate-spin" /> Checking authorization...
											</>
										) : countdown !== null && countdown > 0 ? (
											<>Check authorization ({countdown}s)</>
										) : (
											<>Check authorization</>
										)}
									</Button>
									<Button
										type="button"
										variant="ghost"
										size="sm"
										className="w-full"
										disabled={disabled}
										onClick={resetFlow}
										data-testid="copilot-cancel-login-button"
									>
										Cancel and start over
									</Button>
								</div>
							)}
						</>
					)}

					{status === "complete" && (
						<Alert variant="default" data-testid="copilot-auth-complete">
							<CheckCircle2 className="mt-0.5 h-4 w-4 flex-shrink-0 text-green-600" />
							<AlertTitle>Authorized</AlertTitle>
							<AlertDescription>{message}</AlertDescription>
						</Alert>
					)}
				</div>
			) : (
				<div className="space-y-2">
					<Alert variant="default">
						<Info className="mt-0.5 h-4 w-4 flex-shrink-0 text-blue-600" />
						<AlertTitle>Pre-generated token</AlertTitle>
						<AlertDescription>
							Enter a pre-generated Copilot API token. This is not a GitHub OAuth token or PAT, and it requires the issuing Copilot API host
							in the provider Base URL.
						</AlertDescription>
					</Alert>
					<FormField
						control={form.control}
						name="key.value"
						render={({ field }) => (
							<FormItem>
								<FormLabel>GitHub Copilot Token</FormLabel>
								<FormControl>
									<SecretVarInput {...field} disabled={disabled} data-testid="copilot-api-token" />
								</FormControl>
								<FormMessage />
							</FormItem>
						)}
					/>
				</div>
			)}

			{status === "error" && message && (
				<Alert variant="destructive" data-testid="copilot-auth-status">
					<AlertDescription>{message}</AlertDescription>
				</Alert>
			)}
		</div>
	);
}