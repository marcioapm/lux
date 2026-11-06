// A test file's DOM: happy-dom is registered before load() imports the
// modules under test (the router touches window when it loads), React's act
// environment is on, and the DOM goes away after the file.
import { afterAll, beforeAll } from "bun:test";
import { GlobalRegistrator } from "@happy-dom/global-registrator";

export function withDom(load: () => Promise<unknown>): void {
  beforeAll(async () => {
    GlobalRegistrator.register();
    await load();
    (globalThis as { IS_REACT_ACT_ENVIRONMENT?: boolean }).IS_REACT_ACT_ENVIRONMENT = true;
  });
  afterAll(async () => {
    await GlobalRegistrator.unregister();
  });
}
