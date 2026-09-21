// OS-level notifications when an agent pauses for approval or proposes
// a plan. Closes the "agents that ask before they act" loop — the user
// doesn't need the Nomi window focused to notice that an agent is waiting.
//
// Tauri (desktop): uses @tauri-apps/plugin-notification, which routes
// through the host OS's Notification Center (macOS), org.freedesktop
// notifications (Linux), or toast (Windows). Click deep-links via
// `onAction` + stamped `extra.nomi` (approvals → Approvals tab; plan →
// Chats for that run).
//
// Web (vite dev, Playwright, Scout): falls back to the Notification
// Web API. Permission is requested lazily on the first event so a
// fresh tab doesn't get a permission popup before any agent activity.

import type { PluginListener } from "@tauri-apps/api/core";
import {
  isPermissionGranted as tauriIsGranted,
  onAction as tauriOnAction,
  requestPermission as tauriRequest,
  sendNotification as tauriSend,
} from "@tauri-apps/plugin-notification";
import {
  APPROVAL_NOTIF_EXTRA,
  PLAN_NOTIF_EXTRA,
  notificationActionKind,
  type NotificationActionKind,
} from "./notification-actions";

let permissionState: "default" | "granted" | "denied" = "default";
let permissionPromise: Promise<"granted" | "denied"> | null = null;
let userDisabled = false;

const STORAGE_KEY = "nomi.notifications.disabled";

export type NotificationClickPayload = {
  kind: NotificationActionKind;
  runId?: string;
};

export type NotificationClickHandler = (payload: NotificationClickPayload) => void;

let clickHandler: NotificationClickHandler | null = null;
let actionListener: PluginListener | null = null;
let actionListenPromise: Promise<void> | null = null;

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

function dispatchClick(extra: Record<string, unknown> | undefined): void {
  const kind = notificationActionKind(extra);
  if (!kind) return;
  const runId =
    typeof extra?.run_id === "string" && extra.run_id.trim()
      ? extra.run_id.trim()
      : undefined;
  clickHandler?.({ kind, runId });
}

async function ensureActionListener(): Promise<void> {
  if (!inTauri() || actionListener) return;
  if (actionListenPromise) return actionListenPromise;
  actionListenPromise = (async () => {
    try {
      actionListener = await tauriOnAction((notification) => {
        dispatchClick(notification.extra);
      });
    } catch {
      // Plugin unavailable (tests / stripped builds).
    }
  })();
  await actionListenPromise;
}

/**
 * Register deep-links for notification clicks (Tauri `onAction` + Web
 * Notification `onclick`). Call once from App mount.
 */
export function subscribeNotificationClicks(handler: NotificationClickHandler): () => void {
  clickHandler = handler;
  void ensureActionListener();
  return () => {
    if (clickHandler === handler) clickHandler = null;
  };
}

async function focusWindow(): Promise<void> {
  try {
    window.focus();
  } catch {
    // headless / cross-origin
  }
}

export interface ApprovalNotificationInput {
  capability: string;
  approvalID?: string;
  runID?: string;
}

export interface PlanNotificationInput {
  runID: string;
  goal?: string;
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
  const extra = {
    ...APPROVAL_NOTIF_EXTRA,
    approval_id: input.approvalID ?? "",
    run_id: input.runID ?? "",
  };

  try {
    if (inTauri()) {
      await ensureActionListener();
      tauriSend({ title, body, extra });
    } else if (typeof Notification !== "undefined") {
      const n = new Notification(title, { body, tag: input.approvalID ?? "nomi-approval" });
      n.onclick = () => {
        void focusWindow();
        dispatchClick(extra);
      };
    }
  } catch {
    // Logged at debug level only — notifications are advisory.
  }
}

/** OS notification when a run enters plan_review (initial proposal only). */
export async function notifyPlanProposed(input: PlanNotificationInput): Promise<void> {
  if (userDisabled) return;
  if (!input.runID) return;
  const granted = await ensurePermission();
  if (granted !== "granted") return;

  const goal =
    input.goal && input.goal.trim()
      ? input.goal.trim().length > 80
        ? `${input.goal.trim().slice(0, 77)}…`
        : input.goal.trim()
      : "a plan";
  const title = "Nomi — plan ready for review";
  const body = `Review ${goal}. Click to open.`;
  const extra = {
    ...PLAN_NOTIF_EXTRA,
    run_id: input.runID,
  };

  try {
    if (inTauri()) {
      await ensureActionListener();
      tauriSend({ title, body, extra });
    } else if (typeof Notification !== "undefined") {
      const n = new Notification(title, { body, tag: `nomi-plan-${input.runID}` });
      n.onclick = () => {
        void focusWindow();
        dispatchClick(extra);
      };
    }
  } catch {
    // Advisory only.
  }
}
