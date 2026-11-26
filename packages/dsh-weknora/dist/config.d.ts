/** Plugin configuration: shape, defaults, and load-time validation. */
/** Which tools the plugin registers. Omitted entries keep their default. */
export interface ToolToggles {
    listKnowledgeBases?: boolean;
    search?: boolean;
    readDocument?: boolean;
    ask?: boolean;
}
/** User-supplied configuration, as written in a Cordis patch row. */
export interface Config {
    /** WeKnora API root, with or without the `/api/v1` suffix. */
    baseUrl?: string;
    /** Tenant or platform API key, sent as `X-API-Key`. */
    apiKey?: string;
    /** Workspace id, required only for platform-scoped API keys (`X-Tenant-ID`). */
    tenantId?: string;
    /** Default knowledge-base scope when a call names none. */
    knowledgeBaseIds?: string[];
    /** Default custom agent for `weknora_ask`; omitted uses the RAG pipeline. */
    agentId?: string;
    /** Default and maximum number of chunks a search returns. */
    maxResults?: number;
    /** Per-chunk character budget before the plugin truncates content. */
    maxChunkChars?: number;
    /** Timeout for non-streaming requests. */
    requestTimeoutMs?: number;
    /** Timeout for a streamed answer, which includes model and tool time. */
    chatTimeoutMs?: number;
    /**
     * `public` asks WeKnora for directly loadable file URLs in answers and
     * citations (the default: dsh can render `https://` Markdown images, not
     * `resource://` handles). `handle` keeps the internal references.
     */
    resourceUrls?: 'handle' | 'public';
    /** Tool-name prefix, so two instances can serve two deployments. */
    toolPrefix?: string;
    /** Per-tool registration switches. */
    tools?: ToolToggles;
}
/** Configuration after defaults and validation. */
export interface ResolvedConfig {
    baseUrl: string;
    apiKey: string | undefined;
    tenantId: string | undefined;
    knowledgeBaseIds: string[];
    agentId: string | undefined;
    maxResults: number;
    maxChunkChars: number;
    requestTimeoutMs: number;
    chatTimeoutMs: number;
    resourceUrls: 'handle' | 'public';
    toolPrefix: string;
    tools: Required<ToolToggles>;
}
/** Thrown when a patch row configures this plugin in a way it cannot serve. */
export declare class ConfigError extends Error {
    constructor(violations: string[]);
}
/**
 * Normalize a WeKnora base URL to an API root. Both `https://kb.example.com`
 * and `https://kb.example.com/api/v1` are accepted, because a deployment's
 * documented address is usually the bare origin.
 */
export declare function normalizeBaseUrl(raw: string): string;
/**
 * Apply defaults and reject an unusable row while the plugin loads, so a typo
 * surfaces at boot rather than inside the first tool call the model makes.
 * @param raw - the `config` value from the Cordis row.
 * @returns configuration every consumer can read without re-checking.
 */
export declare function resolveConfig(raw: Config | undefined): ResolvedConfig;
