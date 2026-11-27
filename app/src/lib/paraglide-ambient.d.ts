declare module "#lib/paraglide/messages.js" {
  export type MessageFunction = (
    inputs?: Record<string, string | number>,
    options?: { locale?: string },
  ) => string;

  export const m: Record<string, MessageFunction>;
}

declare module "#lib/paraglide/runtime.js" {
  export type CustomClientStrategyHandler = {
    getLocale: () => Promise<string | undefined> | string | undefined;
    setLocale: (locale: string) => Promise<void> | void;
  };

  export function defineCustomClientStrategy(
    strategy: string,
    handler: CustomClientStrategyHandler,
  ): void;
}
