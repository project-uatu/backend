# Changelog

## [0.2.0](https://github.com/project-uatu/backend/compare/v0.1.0...v0.2.0) (2026-10-22)


### Features

* collect bounded commit history across forges ([#35](https://github.com/project-uatu/backend/issues/35)) ([2ed8451](https://github.com/project-uatu/backend/commit/2ed84516f3696a0ca5eeca8721fcb14f25988578))
* collect gitlab.com and codeberg activity ([#30](https://github.com/project-uatu/backend/issues/30)) ([e6ba2b8](https://github.com/project-uatu/backend/commit/e6ba2b86e3a76e0e8c2a91c0df8459ecf6de1d3d))
* Kubernetes runtime contract and e2e harness ([70f5238](https://github.com/project-uatu/backend/commit/70f5238c4cf5e36050f8f13839afa57108af4482))

## 0.1.0 (2026-10-17)


### ⚠ BREAKING CHANGES

* GET /gh/users/{user}/contributions and GET / are removed, and GET no longer collects missing data. Run uatu collect before reading. Data is stored under a new source key, so previously collected data must be collected again.
* GET /devscores/{user} is removed and collect no longer fetches DevStats data.

### Features

* bootstrap snapshot collection and HTTP API ([4bd1368](https://github.com/project-uatu/backend/commit/4bd1368627d87d4ce6c229db7ff99db190f3542f))
* collect person-centric GitHub contribution history ([45dd2ef](https://github.com/project-uatu/backend/commit/45dd2eff14e696d16549a9ab4cbec2ca9dcfc4eb))
* collect person-centric GitHub contribution history ([#21](https://github.com/project-uatu/backend/issues/21)) ([1f1f0a6](https://github.com/project-uatu/backend/commit/1f1f0a6ff80598f846a69b88db4677aeba51fc62))
* **collector:** implement resumable collection worker ([ebf209e](https://github.com/project-uatu/backend/commit/ebf209e98d2d69007456f949f5bdd7bf3c4112ed))
* **collector:** implement resumable collection worker ([#19](https://github.com/project-uatu/backend/issues/19)) ([afd529c](https://github.com/project-uatu/backend/commit/afd529ce2785ee8ec43c4d8b4165b55298c208fe))
* make the scaffold API read-only ([2859f6f](https://github.com/project-uatu/backend/commit/2859f6f73eacafb6794c2d7075e761cf5ef7f96d)), closes [#2](https://github.com/project-uatu/backend/issues/2)
* remove the DevStats source ([255f636](https://github.com/project-uatu/backend/commit/255f636819f993e2b51fcceeda2e5a6f80ffe0cf)), closes [#2](https://github.com/project-uatu/backend/issues/2)
* **store:** migrate to PostgreSQL person, identity and event schema ([fb643ba](https://github.com/project-uatu/backend/commit/fb643ba9fb03d7e9f14d27ca702ccfe67b1efffe)), closes [#3](https://github.com/project-uatu/backend/issues/3)
* **store:** migrate to PostgreSQL person, identity and event schema ([#15](https://github.com/project-uatu/backend/issues/15)) ([41b88cd](https://github.com/project-uatu/backend/commit/41b88cde77ebd964fea9035de233319c689fda48))


### Bug Fixes

* address GitHub history collection review findings ([e458501](https://github.com/project-uatu/backend/commit/e458501c5132ecba02cef8d35768dc403e68904b))
* **collector:** address resumable collection review findings ([0ce2684](https://github.com/project-uatu/backend/commit/0ce2684a0f4f94bab55afa6bd02d04be6ee8deb2))
* **github:** give history constants explicit separate declarations ([8de19f3](https://github.com/project-uatu/backend/commit/8de19f3f580cd1a00a7aa38a704e1fb12b3dc63f))
* implement identity claim update for event people ([308b66d](https://github.com/project-uatu/backend/commit/308b66d20abb1d6e9828ad8276da78dc40c5da74))
* query to count identity keys using correct id ([60bcb72](https://github.com/project-uatu/backend/commit/60bcb728b45c5f06442bba40e167d70d04ffd958))
* serialize identity event-link updates ([d89c550](https://github.com/project-uatu/backend/commit/d89c550a672a8f5647772a87ccfb346563b7a712))
* **store:** normalize rate-limit reset precision ([c6fe9b9](https://github.com/project-uatu/backend/commit/c6fe9b9aad4564ce31c25e99a57e786d4ac364f9))
* update statement for normalized_events ([5db94ec](https://github.com/project-uatu/backend/commit/5db94ec1fb31dfe2e995b6939bfb7ffe4e1e8fff))
