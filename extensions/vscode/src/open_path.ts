import * as vscode from "vscode";
import { resolveOpenPathFs } from "./open_path_fs";

/** Open a plan-review path in the editor (preview, column One). */
export async function openPathFromPlanReview(pathHint: string): Promise<void> {
  const root = vscode.workspace.workspaceFolders?.[0]?.uri.fsPath;
  const fsPath = resolveOpenPathFs(pathHint, root);
  if (!fsPath) {
    vscode.window.showWarningMessage("Nomi: no path to open");
    return;
  }
  try {
    const doc = await vscode.workspace.openTextDocument(vscode.Uri.file(fsPath));
    await vscode.window.showTextDocument(doc, {
      viewColumn: vscode.ViewColumn.One,
      preview: true,
      preserveFocus: false,
    });
  } catch (err) {
    vscode.window.showErrorMessage(
      `Nomi: could not open ${pathHint}: ${err instanceof Error ? err.message : String(err)}`,
    );
  }
}
