// Discovery mirrors cmd/nomi/client.go: same data-dir layout nomid writes
// at boot (auth.token + api.endpoint JSON).

import * as fs from "node:fs";
import * as os from "node:os";
import * as path from "node:path";

export interface NomiEndpoint {
  url: string;
  port?: string;
}

export interface DiscoveryInput {
  /** vscode.workspace.getConfiguration("nomi").get(...) values */
  apiUrl?: string;
  token?: string;
  dataDir?: string;
  env?: NodeJS.ProcessEnv;
  platform?: NodeJS.Platform;
  homedir?: () => string;
  readFile?: (p: string) => string | undefined;
}

export interface DiscoveredConnection {
  url: string;
  token: string;
  source: {
    url: "setting" | "endpoint-file" | "default";
    token: "setting" | "env" | "token-file";
  };
}

export function resolveDataDir(input: DiscoveryInput = {}): string {
  const env = input.env ?? process.env;
  if (input.dataDir && input.dataDir.trim()) {
    return input.dataDir.trim();
  }
  if (env.NOMI_DATA_DIR && env.NOMI_DATA_DIR.trim()) {
    return env.NOMI_DATA_DIR.trim();
  }
  const home = (input.homedir ?? os.homedir)();
  const platform = input.platform ?? process.platform;
  if (platform === "darwin") {
    return path.join(home, "Library", "Application Support", "Nomi");
  }
  if (platform === "win32") {
    const appdata = env.APPDATA;
    if (appdata) return path.join(appdata, "Nomi");
  }
  if (env.XDG_CONFIG_HOME) {
    return path.join(env.XDG_CONFIG_HOME, "Nomi");
  }
  return path.join(home, ".config", "Nomi");
}

function defaultReadFile(p: string): string | undefined {
  try {
    return fs.readFileSync(p, "utf8");
  } catch {
    return undefined;
  }
}

export function discoverConnection(input: DiscoveryInput = {}): DiscoveredConnection {
  const read = input.readFile ?? defaultReadFile;
  const env = input.env ?? process.env;
  const dataDir = resolveDataDir(input);

  let url = "http://127.0.0.1:8080";
  let urlSource: DiscoveredConnection["source"]["url"] = "default";
  if (input.apiUrl && input.apiUrl.trim()) {
    url = input.apiUrl.trim().replace(/\/$/, "");
    urlSource = "setting";
  } else {
    const raw = read(path.join(dataDir, "api.endpoint"));
    if (raw) {
      try {
        const ep = JSON.parse(raw) as NomiEndpoint;
        if (ep.url) {
          url = ep.url.replace(/\/$/, "");
          urlSource = "endpoint-file";
        }
      } catch {
        // fall through to default
      }
    }
  }

  let token = "";
  let tokenSource: DiscoveredConnection["source"]["token"] = "token-file";
  if (input.token && input.token.trim()) {
    token = input.token.trim();
    tokenSource = "setting";
  } else if (env.NOMI_TOKEN && env.NOMI_TOKEN.trim()) {
    token = env.NOMI_TOKEN.trim();
    tokenSource = "env";
  } else {
    const raw = read(path.join(dataDir, "auth.token"));
    if (raw) {
      token = raw.trim();
      tokenSource = "token-file";
    }
  }

  if (!token) {
    throw new Error(
      "No Nomi auth token: set nomi.token, $NOMI_TOKEN, or run nomid on this machine (auth.token).",
    );
  }

  return {
    url,
    token,
    source: { url: urlSource, token: tokenSource },
  };
}
