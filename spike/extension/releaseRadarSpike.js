// NAME: Release Radar (spike)
// AUTHOR: yurii
// VERSION: 0.0.1
// DESCRIPTION: Spike S7 - prove fetch() reaches our backend and ContextMenu.Item works on artists.

/// <reference path="C:/Users/levch/AppData/Local/spicetify/globals.d.ts" />

// Spike S7. Answers three questions, then gets deleted:
//   1. does a plain fetch() from the Spotify client reach our own backend?  (SPEC C15/C16)
//   2. does Spicetify.ContextMenu.Item register on an artist?               (SPEC D5)
//   3. does the two-predicate pattern flip the menu label by state?         (SPEC FR-4.2)
//
// Deliberately NOT using Spicetify.CosmosAsync - on Spotify >=1.2.31 it proxies
// external URLs through a third-party Cloudflare Worker and drops headers. See SPEC C17.

(function releaseRadarSpike() {
  const API = "http://127.0.0.1:8099";
  const TAG = "[release-radar-spike]";

  // Guard on exactly the namespaces we touch, not a blanket check.
  if (!Spicetify?.ContextMenu?.Item || !Spicetify?.URI || !Spicetify?.showNotification) {
    setTimeout(releaseRadarSpike, 200);
    return;
  }

  // ---------------------------------------------------------------- state
  // Stands in for the real subscription set that v2 fetches from GET /me/subscriptions.
  const KEY = "releaseRadarSpike:subs";
  const load = () => {
    try {
      return new Set(JSON.parse(Spicetify.LocalStorage.get(KEY) || "[]"));
    } catch {
      return new Set();
    }
  };
  const save = (set) => Spicetify.LocalStorage.set(KEY, JSON.stringify([...set]));
  let subs = load();

  // ---------------------------------------------------------------- helpers
  // Parse the id straight out of "spotify:artist:<id>" rather than depending on
  // which URI helper exists in this client version.
  const artistIdOf = (uri) => {
    if (typeof uri !== "string") return null;
    const parts = uri.split(":");
    return parts.length === 3 && parts[1] === "artist" ? parts[2] : null;
  };

  const isArtist = (uris) => {
    if (!Array.isArray(uris) || uris.length === 0) return false;
    try {
      return Spicetify.URI.isArtist(uris[0]);
    } catch {
      return artistIdOf(uris[0]) !== null;
    }
  };

  async function call(action, artistId) {
    const body = {
      action,
      spotify_artist_id: artistId,
      name: document.title || "unknown",
      install_id: "spike-install-0000000000000000",
    };
    console.log(`${TAG} POST ${API}/v1/spike`, body);
    const res = await fetch(`${API}/v1/spike`, {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify(body),
    });
    if (!res.ok) throw new Error(`HTTP ${res.status}`);
    return res.json();
  }

  // ---------------------------------------------------------------- actions
  async function onSubscribe(uris) {
    const id = artistIdOf(uris[0]);
    try {
      const out = await call("subscribe", id);
      subs.add(id);
      save(subs);
      console.log(`${TAG} server replied`, out);
      Spicetify.showNotification(`Spike OK — subscribed ${id}`);
    } catch (err) {
      console.error(`${TAG} FAILED`, err);
      Spicetify.showNotification(`Spike FAILED: ${err.message}`, true, 6000);
    }
  }

  async function onUnsubscribe(uris) {
    const id = artistIdOf(uris[0]);
    try {
      await call("unsubscribe", id);
      subs.delete(id);
      save(subs);
      Spicetify.showNotification(`Spike OK — unsubscribed ${id}`);
    } catch (err) {
      console.error(`${TAG} FAILED`, err);
      Spicetify.showNotification(`Spike FAILED: ${err.message}`, true, 6000);
    }
  }

  // ---------------------------------------------------------------- register
  // Two items, one visible at a time. shouldAdd runs on every menu open, so the
  // label tracks state with no dynamic relabelling. This is the FR-4.2 pattern.
  // Icon names come from the Icon union in globals.d.ts - there is no "bell".
  new Spicetify.ContextMenu.Item(
    "Notify me about releases  [spike]",
    onSubscribe,
    (uris) => isArtist(uris) && !subs.has(artistIdOf(uris[0])),
    "follow"
  ).register();

  new Spicetify.ContextMenu.Item(
    "Stop notifying  [spike]",
    onUnsubscribe,
    (uris) => isArtist(uris) && subs.has(artistIdOf(uris[0])),
    "check"
  ).register();

  console.log(
    `${TAG} loaded. Spotify=${Spicetify.Platform?.version ?? "?"} ` +
      `known subs=${subs.size}. Right-click any artist.`
  );
})();
