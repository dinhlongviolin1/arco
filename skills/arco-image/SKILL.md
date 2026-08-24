---
name: arco-image
description: Send images to, and receive images from, the human operator over Telegram. Use when you have a screenshot/diagram/generated image the operator should see, or when the operator has sent you an image to look at.
---

# arco-image — operator image relay

You are a worker supervised by **arco**. arco bridges images between you and the
human operator's Telegram, in both directions. You never handle the Telegram bot
token — arco holds it and does the sending on your behalf.

## Sending an image to the operator (outbound)

When you produce an image the operator should see — a screenshot, a rendered
diagram, a generated asset, a chart — send it with:

```sh
arco image send <path> [--caption "what this is"]
```

- Run it **from inside your worktree**. arco figures out which session you are
  from the working directory, and posts the image into that session's Telegram
  topic.
- `<path>` is relative to your worktree and must stay inside it.
- Example: `arco image send ./out/diff-preview.png --caption "before vs after"`

## Receiving an image from the operator (inbound)

When the operator sends you a photo, arco saves it into your worktree under:

```
.arco/inbox/
```

Check that directory (e.g. `ls .arco/inbox/`) when the operator refers to "the
image I sent" or "look at this screenshot". Read the file directly — it's a
normal file in your worktree. arco posts a confirmation in the topic naming the
saved path and any caption.

## Notes

- Only the operator's own Telegram account is authorized; images from anyone
  else are dropped by arco.
- If `arco image send` reports "no worker owns worktree", you're not running it
  from inside your worktree — `cd` into it first.
