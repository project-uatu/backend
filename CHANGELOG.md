# Changelog

## 1.0.0 (2026-10-16)


### ⚠ BREAKING CHANGES

* GET /gh/users/{user}/contributions and GET / are removed, and GET no longer collects missing data. Run uatu collect before reading. Data is stored under a new source key, so previously collected data must be collected again.
* GET /devscores/{user} is removed and collect no longer fetches DevStats data.

### Features

* bootstrap snapshot collection and HTTP API ([4bd1368](https://github.com/project-uatu/backend/commit/4bd1368627d87d4ce6c229db7ff99db190f3542f))
* make the scaffold API read-only ([2859f6f](https://github.com/project-uatu/backend/commit/2859f6f73eacafb6794c2d7075e761cf5ef7f96d)), closes [#2](https://github.com/project-uatu/backend/issues/2)
* remove the DevStats source ([255f636](https://github.com/project-uatu/backend/commit/255f636819f993e2b51fcceeda2e5a6f80ffe0cf)), closes [#2](https://github.com/project-uatu/backend/issues/2)
