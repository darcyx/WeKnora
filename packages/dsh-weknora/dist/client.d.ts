/** Minimal WeKnora REST client: JSON endpoints plus the SSE chat streams. */
import type { ResolvedConfig } from './config.ts';
/** One retrieval hit, as returned by WeKnora's `SearchResult`. */
export interface SearchResult {
    id?: string;
    content?: string;
    knowledge_id?: string;
    chunk_index?: number;
    knowledge_title?: string;
    knowledge_filename?: string;
    score?: number;
    match_type?: number | string;
    [key: string]: unknown;
}
/** A knowledge base as listed by `GET /knowledge-bases`. */
export interface KnowledgeBaseSummary {
    id?: string;
    name?: string;
    description?: string;
    [key: string]: unknown;
}
/** One stored chunk as returned by `GET /chunks/:knowledge_id`. */
export interface ChunkRecord {
    id?: string;
    content?: string;
    chunk_index?: number;
    knowledge_id?: string;
    [key: string]: unknown;
}
/** A document as returned by the by-name search and by `GET /knowledge/:id`. */
export interface DocumentRecord {
    id?: string;
    title?: string;
    file_name?: string;
    file_type?: string;
    description?: string;
    knowledge_base_id?: string;
    knowledge_base_name?: string;
    [key: string]: unknown;
}
/** A page of chunks plus its pagination envelope. */
export interface ChunkPage {
    chunks: ChunkRecord[];
    total: number;
    page: number;
    pageSize: number;
}
/** The assembled outcome of one streamed answer. */
export interface StreamedAnswer {
    answer: string;
    references: SearchResult[];
    sessionId: string;
    toolCalls: string[];
}
/** Raised for any non-2xx response or transport failure, with the key redacted. */
export declare class WeknoraApiError extends Error {
    readonly status: number | undefined;
    constructor(message: string, status?: number);
}
export declare class WeknoraClient {
    private readonly config;
    /**
     * A knowledge-base-restricted API key rejects `resource_urls=public` with
     * 403. After the first such refusal this client stays on handle mode so
     * later calls do not pay the same round trip.
     */
    private publicModeBlocked;
    constructor(config: ResolvedConfig);
    private headers;
    private resourceMode;
    private url;
    private isPublicModeForbidden;
    private fetchJson;
    private fetchJsonOnce;
    /** List the knowledge bases this credential can retrieve from. */
    listKnowledgeBases(signal: AbortSignal): Promise<KnowledgeBaseSummary[]>;
    /** Hybrid (vector + keyword) retrieval across one or more knowledge bases. */
    search(input: {
        query: string;
        knowledgeBaseIds: string[];
        knowledgeIds: string[];
    }, signal: AbortSignal): Promise<SearchResult[]>;
    /**
     * Find documents by title or filename. Unlike `search` this spans every
     * knowledge base the credential can see and needs no scope, so it is what
     * answers "where is the document called X".
     */
    findDocuments(input: {
        keyword: string;
        limit: number;
    }, signal: AbortSignal): Promise<DocumentRecord[]>;
    /** A document's metadata, including the generated summary WeKnora stores as `description`. */
    getDocument(knowledgeId: string, signal: AbortSignal): Promise<DocumentRecord>;
    /** One page of a document's stored chunks, in storage order. */
    listChunks(input: {
        knowledgeId: string;
        page: number;
        pageSize: number;
    }, signal: AbortSignal): Promise<ChunkPage>;
    /** Create a conversation container to hold one or more asked questions. */
    createSession(title: string, signal: AbortSignal): Promise<string>;
    /**
     * Ask a question and assemble the streamed answer. `agentId` selects the
     * ReAct pipeline (`/agent-chat`); without it the RAG pipeline answers
     * (`/knowledge-chat`).
     */
    ask(input: {
        sessionId: string;
        query: string;
        knowledgeBaseIds: string[];
        agentId: string | undefined;
        webSearch: boolean;
    }, signal: AbortSignal): Promise<StreamedAnswer>;
}
