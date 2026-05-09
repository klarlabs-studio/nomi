# Recipe: Inbox Triage (Telegram)

A triage assistant bound to a Telegram chat. You forward / DM messages
to a bot; the agent classifies, drafts a reply, and asks for approval
before sending. The Telegram plugin is the only first-party connector
shipping today, so this is the minimum-viable "personal AI" recipe.

## What it does

- Receives inbound messages over the Telegram plugin (webhook or
  long-polling, configured in the plugin tab).
- Plans a triage path: classify → draft reply → request approval.
- Sends the reply only after you approve it from the desktop app
  or the same Telegram chat.
- Persists the conversation to SQLite for later reference.

## Prereqs

1. A Telegram bot — create one via [@BotFather](https://t.me/BotFather),
   copy the token.
2. The Telegram plugin enabled in **Settings → Plugins** with that
   token. The plugin tab shows the bot's @handle once configured.
3. Your own Telegram account chatting with the bot at least once so
   the bot has somewhere to deliver replies.

## Apply

```bash
nomi seed examples/inbox-triage/seed.yaml
```

Then in the desktop app:
- Open **Plugins → Telegram → Connections** and bind the
  "Inbox Triage" assistant to the bot.
- DM the bot a message like
  *"Should I respond to this email? It says: '...'"* and watch the
  approval card appear in the desktop UI.

## Files

- [`seed.yaml`](seed.yaml) — provider + Inbox Triage assistant.
  The Telegram bot token still configures via the Plugins UI;
  seed manifests don't carry plugin secrets by design.
