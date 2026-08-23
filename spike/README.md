# Spike S7 — throwaway

Answers three questions empirically, then gets deleted. Do not build on this code.

1. Does a plain `fetch()` from a Spicetify extension reach our own backend?  (SPEC C15/C16)
2. Does `Spicetify.ContextMenu.Item` register on an artist?                  (SPEC D5)
3. Does the two-predicate pattern flip the menu label by state?              (SPEC FR-4.2)

## Run

```
cd spike/server && go run .        # listens on 127.0.0.1:8099
```

The extension is already installed and applied:

```
spicetify config extensions releaseRadarSpike.js
spicetify apply
```

Then right-click any artist in Spotify -> "Notify me about releases  [spike]".

A real pass prints `==> SPIKE PASSED`. A local `curl` prints
`--- local smoke test ---` instead, so the two can never be confused.

## Teardown

```
spicetify config extensions releaseRadarSpike.js-
spicetify apply
rm -rf spike
```

Delete this whole directory once S8 builds the real API.
