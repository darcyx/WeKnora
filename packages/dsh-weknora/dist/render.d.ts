/** Pure projections from a tool's canonical value to model-facing text. */
/** Clip text to a character budget, reporting whether anything was dropped. */
export declare function clip(text: string, maxChars: number): {
    text: string;
    truncated: boolean;
};
/** Human-readable score, tolerant of a backend that omits it. */
export declare function formatScore(score: number): string;
/**
 * Name a retrieval scope without spending the model's context on it. A
 * deployment can hold dozens of knowledge bases, and spelling out every id on
 * every search costs far more than the count is worth; the ids stay in the
 * tool's canonical value for anything that needs them.
 */
export declare function describeScope(ids: string[]): string;
