# Getting started

This is a shared front door for a handful of small apps. You sign in once, pick
which apps you want, and each one is yours alone — your own workout log, your
own reading list, nobody else's data mixed in with it.

You need the address someone gave you. It usually looks like
`http://something.local:8080` or a Tailscale address. It is normally *not* on
the public internet, so if it does not load, check you are on the right network.

## 1. Sign up

Open the address and choose **Sign up**.

- **Username** becomes part of every URL you use: `/alice/readerr/`. Pick
  something short and lowercase. A few names are taken by the system (`admin`,
  `login`, `apps`, and similar) and will be rejected.
- **Password** must be at least 10 characters. That is the only rule — no
  "must contain a symbol" nonsense. Length is what matters, so a few unrelated
  words beat a short mangled one.

If sign-up is turned off, you will not see the option. Ask whoever runs the
server to make you an account.

## 2. Save your recovery passphrase — this is the important step

Immediately after signing up you are shown six words:

```
meadow-cobalt-jigsaw-hornet-fresco-lantern
```

**This is shown once and never again.** Nobody can look it up later — it is
stored scrambled, in a way that cannot be reversed, so the person running the
server genuinely cannot recover it for you.

There is no email on this system. No "forgot password" link will ever arrive in
your inbox, because there is no inbox. Those six words *are* the password reset.

Put them somewhere you will still have in a year:

- your password manager, in the notes field of the entry for this site (best)
- a piece of paper in a drawer (genuinely fine)
- not a screenshot in your photo roll, and not a note in the app itself

If you lose both your password and your passphrase, an administrator has to
reset your account by hand. Your app data survives that — it is your login that
is gone — but it is an awkward conversation and it is entirely avoidable.

You can copy the passphrase from the screen before continuing. Take the extra
ten seconds now.

## 3. Add your apps

After signing in you land on the app chooser. Nothing is set up yet — apps are
opt-in.

Click **Add** on the ones you want. That creates a fresh, empty copy of that app
just for you. Adding an app takes a moment and then it appears in your list.

You can add more later. Adding an app you have used before does not wipe
anything; removing one is the action that asks you to confirm.

## 4. Use them

Click an app to open it at `/<your-name>/<app>/`. From there it is just the app
— it does not know or care that it is behind a login.

Worth knowing:

- **Your data lives in two places.** These are local-first apps: the copy in
  your browser is the real one, and the server holds a synced backup. That is
  why they keep working when the network does not.
- **Sync happens automatically**, and each app has a Sync button if you want to
  force it. If you use an app on your laptop and your phone, sync on both to
  keep them together.
- **Offline works.** Once an app has loaded, it keeps working with no
  connection. Changes sync when you are back.
- **You can install them.** Both apps are installable as PWAs — your browser
  will offer "Install" or "Add to Home Screen". Do this per app.
- **Bookmark the app itself**, not the chooser. `/<your-name>/readerr/` goes
  straight there.
- **The first load after a while is slightly slow.** Apps you have not used
  recently are shut down to save memory and start again on demand. It takes
  about a tenth of a second; you will mostly not notice.

## 5. Changing your password

**Account** → change password. You need your current password.

Changing it **signs you out everywhere else** — every other browser, phone and
tab. That is intentional: it is the mechanism that gets an old device out of
your account. You will need to sign in again on each one.

Your recovery passphrase does not change when your password does.

## 6. If you forget your password

Go to **Sign in** → **Forgot password**. You will be asked for your username and
your six-word recovery passphrase, then for a new password.

The passphrase is forgiving about how you type it: capitals do not matter, and
spaces work as well as dashes. `Meadow Cobalt JIGSAW hornet fresco lantern` is
accepted.

Guessing is not: after a few wrong attempts the system starts making you wait,
and the wait doubles each time. This is deliberate and there is no way around it
— wait it out, or go find the passphrase.

Once the reset succeeds you are signed out of every device, same as a normal
password change.

**If you have lost the passphrase too**, an administrator can reset your account
manually. Your app data is untouched by this.

## Things that will surprise you

**These apps are yours alone.** There is no sharing, no collaboration, no
sending an item to someone else. If two of you use the reading list, you have
two entirely separate reading lists.

**On a shared computer, sign out and close the browser.** The apps keep their
data *in the browser*, and signing out of the gateway does not erase it — the
next person on that browser profile could open the app's storage directly. On a
family or public machine, use a private window or your own browser profile.

**Signing out does not delete anything.** Your data stays on the server and in
your browser. Sign back in and it is all there.

**Removing an app removes the data.** That one is real, and the confirmation
dialog is not being dramatic.

**Nobody can read your data through the apps** — every user gets a genuinely
separate copy. The person running the server can, though: they have the files.
That is true of every self-hosted service and is worth knowing rather than
assuming otherwise.
