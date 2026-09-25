import { Button } from "@/components/ui/button";
import { ConfigSyncAlert } from "@/components/ui/configSyncAlert";
import { Form } from "@/components/ui/form";
import { Tooltip, TooltipContent, TooltipProvider, TooltipTrigger } from "@/components/ui/tooltip";
import { getErrorMessage } from "@/lib/store";
import {
	useCreateProviderKeyMutation,
	useGetProviderKeysQuery,
	useUpdateProviderKeyMutation,
	useRefreshProviderModelsMutation,
} from "@/lib/store/apis/providersApi";
import { ModelProvider, ModelProviderKey } from "@/lib/types/config";
import { modelProviderKeySchema } from "@/lib/types/schemas";
import { RbacOperation, RbacResource, useRbac } from "@enterprise/lib";
import { zodResolver } from "@hookform/resolvers/zod";
import { Save } from "lucide-react";
import { useCallback, useEffect, useState } from "react";
import { useForm } from "react-hook-form";
import { toast } from "sonner";
import { v4 as uuid } from "uuid";
import { z } from "zod";
import { ApiKeyFormFragment } from "../fragments";
import { type CopilotCredential } from "../fragments/copilotAuthForm";
import { stripDatabricksAuthDiscriminator } from "./providerKeyForm.utils";
interface Props {
	provider: ModelProvider;
	keyId: string | null;
	onCancel: () => void;
	onSave: () => void;
	onCreated?: () => void;
}

// Create a simple form schema using only ModelProviderKeySchema
const providerKeyFormSchema = z.object({
	key: modelProviderKeySchema,
});

type ProviderKeyFormValues = z.infer<typeof modelProviderKeySchema>;

export default function ProviderKeyForm({ provider, keyId, onCancel, onSave, onCreated }: Props) {
	const hasUpdateProviderAccess = useRbac(RbacResource.ModelProvider, RbacOperation.Update);
	const [createProviderKey, { isLoading: isCreatingProviderKey }] = useCreateProviderKeyMutation();
	const [updateProviderKey, { isLoading: isUpdatingProviderKey }] = useUpdateProviderKeyMutation();
	const { data: keys = [] } = useGetProviderKeysQuery(provider.name);
	const [savedKeyId, setSavedKeyId] = useState(keyId);
	const [refreshModels, { isLoading: refreshingModels }] = useRefreshProviderModelsMutation();
	const [refreshStatus, setRefreshStatus] = useState("");
	const isCopilot = (provider.custom_provider_config?.base_provider_type ?? provider.name) === "github-copilot";
	const isEditing = savedKeyId !== null;
	const currentKey = savedKeyId ? keys.find((k) => k.id === savedKeyId) : undefined;

	const form = useForm({
		resolver: zodResolver(providerKeyFormSchema),
		mode: "onChange",
		reValidateMode: "onChange",
		defaultValues: {
			key: (currentKey as ProviderKeyFormValues) ?? {
				id: uuid(),
				name: "",
				models: ["*"],
				blacklisted_models: [],
				weight: 1.0,
				enabled: true,
				...(isCopilot ? { github_copilot_key_config: { auth_mode: "oauth" as const, auth_client_id: "" } } : {}),
			},
		},
	});

	// Reset form when currentKey arrives (handles late async resolution)
	// Skip reset if user has unsaved edits to avoid discarding changes during background refetches
	useEffect(() => {
		if (!isEditing || !currentKey || form.formState.isDirty) return;
		form.reset({ key: currentKey as ProviderKeyFormValues });
	}, [isEditing, currentKey, form]);

	// Trigger validation on mount when editing existing data
	useEffect(() => {
		if (isEditing) {
			form.trigger();
		}
	}, [isEditing, form]);

	const getTooltipContent = useCallback(() => {
		if (!hasUpdateProviderAccess) {
			return "You do not have permission to modify provider keys";
		}
		if (!form.formState.isValid && form.formState.errors.root?.message) {
			return form.formState.errors.root?.message;
		}
		if (!form.formState.isDirty) {
			return "No changes made";
		}
		return null;
	}, [form?.formState.errors, form?.formState.isValid, form?.formState.isDirty, hasUpdateProviderAccess]);

	async function refreshSavedModels() {
		try {
			const refreshed = await refreshModels(provider.name).unwrap();
			if (refreshed.some((key) => key.status === "list_models_failed")) {
				setRefreshStatus("Credential saved. Model discovery failed for one or more keys.");
				return false;
			}
			setRefreshStatus("Models refreshed.");
			return true;
		} catch (error) {
			setRefreshStatus(`Credential saved. Model refresh failed: ${getErrorMessage(error)}`);
			return false;
		}
	}
	const onSubmit = async (value: { key: ProviderKeyFormValues }, closeAfter = true) => {
		if (isEditing && !currentKey) return;
		// Strip internal _auth_type fields before sending to API
		const key = { ...value.key };
		if (key.azure_key_config) {
			const { _auth_type, ...rest } = key.azure_key_config;
			key.azure_key_config = rest;
		}
		if (key.vertex_key_config) {
			const { _auth_type, ...rest } = key.vertex_key_config;
			key.vertex_key_config = rest;
		}
		if (key.bedrock_key_config) {
			const { _auth_type, ...rest } = key.bedrock_key_config;
			key.bedrock_key_config = rest;
		}
		if (key.bedrock_mantle_key_config) {
			const { _auth_type, ...rest } = key.bedrock_mantle_key_config;
			key.bedrock_mantle_key_config = rest;
		}
		if (key.databricks_key_config) {
			key.databricks_key_config = stripDatabricksAuthDiscriminator(key.databricks_key_config);
		}
		const mutation = isEditing
			? updateProviderKey({
					provider: provider.name,
					keyId: currentKey!.id,
					key: key as ModelProviderKey,
				})
			: createProviderKey({
					provider: provider.name,
					key: key as ModelProviderKey,
				});

		return mutation
			.unwrap()
			.then(async (saved) => {
				setSavedKeyId(saved.id);
				form.reset({ key: saved as ProviderKeyFormValues });
				if (!isEditing) onCreated?.();
				const refreshed = !isCopilot || (await refreshSavedModels());
				if (closeAfter && refreshed) onSave();
				return true;
			})
			.catch((err) => {
				if (err?.status === 409) {
					form.setError("key.name", { message: getErrorMessage(err) });
					return;
				}
				toast.error(isEditing ? "Error updating key" : "Error creating key", {
					description: getErrorMessage(err),
				});
				return false;
			});
	};

	async function onCopilotAuthorized(credential: CopilotCredential) {
		form.setValue("key.value", { value: credential.accessToken, ref: "" }, { shouldDirty: true, shouldValidate: true });
		// Persisted so the refresh worker can renew unattended. Absent for apps whose
		// tokens never expire, in which case the stored pair is simply left unset.
		form.setValue(
			"key.github_copilot_key_config.refresh_token",
			credential.refreshToken ? { value: credential.refreshToken, ref: "" } : undefined,
			{ shouldDirty: true },
		);
		form.setValue("key.github_copilot_key_config.token_expires_at", credential.expiresAt, { shouldDirty: true });
		if (!form.getValues("key.name").trim()) form.setValue("key.name", "GitHub Copilot", { shouldDirty: true });
		if (!(await form.trigger()) || !(await onSubmit(providerKeyFormSchema.parse(form.getValues()), false)))
			throw new Error("Could not save credential");
	}

	return (
		<Form {...form}>
			<form onSubmit={form.handleSubmit((values) => onSubmit(values).then(() => {}))} className="flex grow flex-col gap-6 pt-4">
				<div className="grow px-4 md:px-8">
					<ApiKeyFormFragment
						control={form.control}
						providerName={provider.name}
						baseProviderType={provider.custom_provider_config?.base_provider_type}
						form={form}
						onCopilotAuthorized={onCopilotAuthorized}
						authDisabled={!hasUpdateProviderAccess || isCreatingProviderKey || isUpdatingProviderKey}
						onRefreshModels={refreshSavedModels}
						refreshingModels={refreshingModels}
					/>
					{refreshStatus && (
						<p role="status" className="mt-3 text-sm" data-testid="copilot-model-refresh-status">
							{refreshStatus}
						</p>
					)}
					{isEditing && currentKey?.config_hash && <ConfigSyncAlert className="mt-4" />}
				</div>
				<div className="bg-card sticky bottom-0 border-t px-4 py-4 md:px-8">
					<div className="flex justify-end space-x-3">
						<Button type="button" variant="outline" onClick={onCancel} data-testid="key-cancel-btn">
							Cancel
						</Button>
						<TooltipProvider>
							<Tooltip>
								<TooltipTrigger asChild>
									<span>
										<Button
											type="submit"
											disabled={!form.formState.isDirty || !hasUpdateProviderAccess}
											isLoading={form.formState.isSubmitting || isCreatingProviderKey || isUpdatingProviderKey}
											data-testid="key-save-btn"
										>
											<Save className="h-4 w-4 shrink-0" />
											Save
										</Button>
									</span>
								</TooltipTrigger>
								{getTooltipContent() && <TooltipContent>{getTooltipContent()}</TooltipContent>}
							</Tooltip>
						</TooltipProvider>
					</div>
				</div>
			</form>
		</Form>
	);
}