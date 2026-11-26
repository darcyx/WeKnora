/**
 * dsh-weknora: a DeepSeek Harness plugin that gives the agent retrieval,
 * document reading and composed answers from a WeKnora knowledge base.
 * @module dsh-weknora
 */
import { WeknoraClient } from "./client.js";
import { resolveConfig } from "./config.js";
import { createTools } from "./tools.js";
export const name = 'dsh-weknora';
/** Cordis waits for the tool registry before applying this plugin. */
export const inject = ['tools'];
export { ConfigError, normalizeBaseUrl, resolveConfig } from "./config.js";
export { WeknoraApiError, WeknoraClient } from "./client.js";
export { createTools } from "./tools.js";
/**
 * Register the configured tools. Each registration is an effect, so unloading
 * or reconfiguring the plugin withdraws the tools without a restart.
 * @param ctx - the Cordis context, with `ctx.tools` injected.
 * @param config - the plugin's `config` row; validated here so a typo fails the load.
 */
export function apply(ctx, config) {
    const resolved = resolveConfig(config);
    const client = new WeknoraClient(resolved);
    const registered = [];
    for (const definition of createTools(client, resolved)) {
        ctx.tools.register(definition);
        registered.push(definition.name);
    }
    ctx.logger?.info(`dsh-weknora: registered ${registered.join(', ')} against ${resolved.baseUrl}`);
}
//# sourceMappingURL=index.js.map