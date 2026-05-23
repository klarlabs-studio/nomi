// OS-level notifications when an agent pauses for approval. Closes the
// "agents that ask before they act" loop — the user doesn't need the
// Nomi window focused to notice that an agent is waiting.
//
// Tauri (desktop): uses @tauri-apps/plugin-notification, which routes
// through the host OS's Notification Center (macOS), org.freedesktop
// notifications (Linux), or toast (Windows).
//
// Web (vite dev, Playwright, Scout): falls back to the Notification
// Web API. Permission is requested lazily on the first event so a
// fresh tab doesn't get a permission popup before any agent activity.

import {
  isPermissionGranted as tauriIsGranted,
  requestPermission as tauriRequest,
  sendNotification as tauriSend,
} from "@tauri-apps/plugin-notification";

let permissionState: "default" | "granted" | "denied" = "default";
let permissionPromise: Promise<"granted" | "denied"> | null = null;
let userDisabled = false;

const STORAGE_KEY = "nomi.notifications.disabled";

if (typeof window !== "undefined") {
  try {
    userDisabled = window.localStorage.getItem(STORAGE_KEY) === "1";
  } catch {
    userDisabled = false;
  }
}

export function setNotificationsEnabled(enabled: boolean): void {
  userDisabled = !enabled;
  try {
    if (enabled) {
      window.localStorage.removeItem(STORAGE_KEY);
    } else {
      window.localStorage.setItem(STORAGE_KEY, "1");
    }
  } catch {
    // localStorage unavailable; in-memory toggle is enough for this session.
  }
}

export function notificationsEnabled(): boolean {
  return !userDisabled;
}

function inTauri(): boolean {
  return (
    typeof window !== "undefined" &&
    typeof (window as unknown as { __TAURI_INTERNALS__?: unknown }).__TAURI_INTERNALS__ !==
      "undefined"
  );
}

async function ensurePermission(): Promise<"granted" | "denied"> {
  if (permissionState !== "default") return permissionState as "granted" | "denied";
  if (permissionPromise) return permissionPromise;

  permissionPromise = (async () => {
    if (inTauri()) {
      try {
        if (await tauriIsGranted()) {
          permissionState = "granted";
          return "granted";
        }
        const result = await tauriRequest();
        permissionState = result === "granted" ? "granted" : "denied";
      } catch {
        permissionState = "denied";
      }
    } else if (typeof Notification !== "undefined") {
      if (Notification.permission === "granted") {
        permissionState = "granted";
      } else if (Notification.permission === "denied") {
        permissionState = "denied";
      } else {
        try {
          const r = await Notification.requestPermission();
          permissionState = r === "granted" ? "granted" : "denied";
        } catch {
          permissionState = "denied";
        }
      }
    } else {
      permissionState = "denied";
    }
    return permissionState as "granted" | "denied";
  })();

  return permissionPromise;
}

export interface ApprovalNotificationInput {
  capability: string;
  approvalID?: string;
  runID?: string;
}

// notifyApprovalRequested fires a single OS notification for an
// `approval.requested` event. Quiet failure: a denied permission, a
// missing Notification API, or a user-disabled toggle all return
// without error — the in-app Approvals tab is still the
// load-bearing surface.
export async function notifyApprovalRequested(input: ApprovalNotificationInput): Promise<void> {
  if (userDisabled) return;
  const granted = await ensurePermission();
  if (granted !== "granted") return;

  const title = "Nomi — approval needed";
  const body = `An assistant is asking to use ${input.capability}. Click to review.`;

  try {
    if (inTauri()) {
      await tauriSend({ title, body });
    } else if (typeof Notification !== "undefined") {
      const n = new Notification(title, { body, tag: input.approvalID ?? "nomi-approval" });
      // Focus the window when the user clicks the notification; the
      // Approvals tab is the in-app continuation.
      n.onclick = () => {
        try {
          window.focus();
        } catch {
          // headless / cross-origin: nothing we can do; the in-app
          // Approvals badge still shows the pending count.
        }
      };
    }
  } catch {
    // Logged at debug level only — notifications are advisory.
  }
}
