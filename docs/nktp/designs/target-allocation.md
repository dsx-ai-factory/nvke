# Required target allocation

Scraping workloads in NVKE telemetry pipeline (NKTP) require a Target Allocator. Allocation determines
which Collector owns each scrape target.

- Watchdog assigns each target to one replica to avoid duplicate scraping.
- Large-target metrics uses the same ownership rule across its replicas.
- The DaemonSet assigns targets to Collectors on the same node.

Disabling or removing target allocation must fail Helm rendering with a clear
message. Grant discovery permissions to the Target Allocator service account.
Collectors retain the permissions needed to scrape their assigned endpoints.

Watchdog monitors the large-target Target Allocator through its own allocated
scrape jobs. The large-target Collectors scrape application targets.
