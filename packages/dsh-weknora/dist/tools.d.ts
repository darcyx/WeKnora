/** The model-facing tools this plugin contributes to `ctx.tools`. */
import type { WeknoraClient } from './client.ts';
import type { ResolvedConfig } from './config.ts';
import type { ToolDefinition } from './harness.ts';
/** Assemble the four tool definitions for one configured deployment. */
export declare function createTools(client: WeknoraClient, config: ResolvedConfig): ToolDefinition[];
