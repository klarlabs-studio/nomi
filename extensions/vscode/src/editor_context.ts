// Gather ephemeral IDE state for nomi run create. Paths + selection only —
// never dump whole buffers. Secret-looking paths are filtered client-side
// (server also sanitizes).

export interface EditorSelectionPayload {
  start_line: number;
  end_line: number;
  text: string;
}

export interface EditorActivePayload {
  path: string;
  language_id?: string;
  selection?: EditorSelectionPayload;
}

export interface EditorContextPayload {
  source: "vscode";
  workspace_folders: string[];
  open_tabs: string[];
  active?: EditorActivePayload;
}

const MAX_TABS = 20;
const MAX_FOLDERS = 5;
const MAX_SELECTION_CHARS = 4000;

const SECRET_BASENAME =
  /^(\.env(\..+)?|credentials(\.json)?|\.npmrc|\.pypirc|id_rsa|id_ed25519|id_ecdsa|id_dsa|auth\.token|.*\.(pem|key|p12))$/i;

export function isSecretPath(p: string): boolean {
  const base = p.split(/[/\\]/).pop() ?? p;
  if (SECRET_BASENAME.test(base)) return true;
  if (base === ".ssh" || base === ".aws" || base === ".gnupg") return true;
  const lower = p.toLowerCase().replace(/\\/g, "/");
  if (
    lower.includes("/.ssh/") ||
    lower.includes("/.aws/") ||
    lower.endsWith("/.ssh") ||
    lower.endsWith("/.aws")
  ) {
    return true;
  }
  if (/secret|passwd|password/i.test(base)) return true;
  return false;
}

export function truncateSelection(text: string, max = MAX_SELECTION_CHARS): string {
  if (text.length <= max) return text;
  return `${text.slice(0, max)}\n…[truncated]`;
}

/** Pure helper for tests — filters + caps lists the extension gathers. */
export function buildEditorContext(input: {
  workspaceFolders: string[];
  openTabs: string[];
  active?: {
    path: string;
    languageId?: string;
    selectionText?: string;
    startLine?: number;
    endLine?: number;
  };
}): EditorContextPayload | undefined {
  const folders = input.workspaceFolders
    .map((f) => f.trim())
    .filter((f) => f && !isSecretPath(f))
    .slice(0, MAX_FOLDERS);
  const tabs = input.openTabs
    .map((t) => t.trim())
    .filter((t) => t && !isSecretPath(t))
    .slice(0, MAX_TABS);

  let active: EditorActivePayload | undefined;
  if (input.active?.path && !isSecretPath(input.active.path)) {
    active = {
      path: input.active.path,
      language_id: input.active.languageId,
    };
    const sel = input.active.selectionText?.trim();
    if (sel) {
      active.selection = {
        start_line: input.active.startLine ?? 1,
        end_line: input.active.endLine ?? 1,
        text: truncateSelection(sel),
      };
    }
  }

  if (folders.length === 0 && tabs.length === 0 && !active) {
    return undefined;
  }
  return {
    source: "vscode",
    workspace_folders: folders,
    open_tabs: tabs,
    active,
  };
}
