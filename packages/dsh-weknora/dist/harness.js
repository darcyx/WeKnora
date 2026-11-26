/**
 * Structural types for the slice of the DeepSeek Harness contract this plugin
 * uses. Declaring them here keeps the package free of runtime dependencies on
 * harness internals: the shipped code only ever hands `ctx.tools.register()` a
 * plain object, so a harness release that adds optional definition fields does
 * not require a republish of this plugin.
 *
 * The shapes mirror `@deepseek-ai/dsh-tools` (`ToolDefinition`) and
 * `@deepseek-ai/dsh-llm` (`ContentBlock`, `ToolSchema`) as of dsh 0.1.0-rc.8.
 */
export {};
//# sourceMappingURL=harness.js.map