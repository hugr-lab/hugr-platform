# Specs

One lightweight spec per feature, `NNN-slug/spec.md` from `TEMPLATE.md`. The owner accepts a spec before code is
written. One branch per spec. Set the status to `implemented` when it lands, and supersede a spec rather than
rewrite history.

| spec | what | status |
| --- | --- | --- |
| [001](001-control-plane/spec.md) | the operator: node groups, a node's entry, the fleet's state | accepted |
| [002](002-front/spec.md) | the front: one address for the clients, every call on its node | implemented |
| [003](003-console/spec.md) | the console: one place to run the platform | accepted |
| [004](004-demo/spec.md) | the demo: the platform in one cluster (kind, minikube, AKS by hand) | implemented |
| [005](005-packaging/spec.md) | packaging: one Helm chart, then the Azure Managed Application | accepted |
