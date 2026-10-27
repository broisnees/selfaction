---
title: Serving actions/cache from a laptop
description: What actions/cache actually sends over the wire, the restore-key rule that is easy to get wrong, and how selfaction stores it all in SQLite and plain files.
date: 2026-10-27
---

`actions/cache` looks simple from a workflow: a path, a key, maybe some restore keys. Underneath, it talks to an HTTP service through a small protocol. To run workflows locally with caching that persists, selfaction serves that protocol itself.

## The protocol

When a job has `ACTIONS_CACHE_URL` set and no newer cache service configured, `@actions/cache`, the library behind `actions/cache`, `setup-go` and `setup-node`, speaks version 1 of the cache API. It is the same version GitHub Enterprise Server uses. A save and a restore look like this:

```txt
GET   …/_apis/artifactcache/cache?keys=deps-Linux-ae04…,deps-Linux-&version=3f9c…
      204 No Content                     a miss
POST  …/_apis/artifactcache/caches       {"key":"deps-Linux-ae04…","version":"3f9c…"}
      {"cacheId": 1}
PATCH …/_apis/artifactcache/caches/1     Content-Range: bytes 33554432-67108863/*
PATCH …/_apis/artifactcache/caches/1     Content-Range: bytes 0-33554431/*
POST  …/_apis/artifactcache/caches/1     {"size": 67108864}
```

The `version` is a hash of the cached paths and the compression method, so a key saved by one configuration is never restored into another. The chunks arrive several at a time and in any order, which is why selfaction writes each one at its offset in the file rather than appending.

## The restore-key rule

The rule is easy to get almost right. For each key in order, an exact match wins. Otherwise, the newest entry whose key starts with that key is used, and only then does the next key get a turn. That ordering matters:

```txt
keys = npm-linux-, npm-linux-aaa
```

Even if `npm-linux-aaa` exists, a newer `npm-linux-bbb` wins, because the first key, a prefix, already matched. selfaction's tests check exactly this case, along with the exact match, a plain prefix and a miss.

## Storage

Each store is a directory holding an SQLite index and one plain file per archive. SQLite runs in write-ahead-log mode, so `selfaction cache prune` can delete entries while the server is answering jobs. A unique index on scope, version and key makes entries immutable, as they are on GitHub: saving a key that already exists, or one that another job is still uploading, is refused, and `actions/cache` treats that as a warning, not a failure.

The scope is the repository, carried in the URL itself:

```txt
ACTIONS_CACHE_URL=http://10.0.0.5:7979/<token>/broisnischal.selfaction/
```

Two projects that both use a key like `node-modules-linux` therefore never restore each other's files, and the token never needs to be sent as a header.

## Does it hold up?

We tested against the real actions rather than against our own assumptions:

- **`actions/cache` v4, v5 and v6** all restored an entry saved by an earlier run, reporting `cache-hit=true`.
- **A changed dependency file** missed the exact key, fell back to the restore key, reported `cache-hit=false`, then saved the new key.
- **selfaction's own CI, run locally twice**: `actions/setup-go` took 15.4 seconds the first time and 0.7 seconds the second, restoring Go's module cache from the store.

Retention keeps the store bounded: entries unused for a week go first, then the least recently used ones beyond 10 GB per repository. The commands for looking inside are in [Cleaning up](/docs/guides/cleaning-up/).
