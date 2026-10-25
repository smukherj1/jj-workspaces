# MVP Implementation Roadmap

Technical roadmap to implement [DESIGN.md](../spec/DESIGN.md).

## Strategy

- Very brief and high level description of technical milestones to build out the
  initial version (excluding any future work items).

- Incrementally build capabilities through the stack in each milestone and sub-milestone.
  e.g., First milestone would be have a basic cli, daemon and service working that can
  connect and call each other but no functionality yet. Sub-milestones to create cli,
  then daemon and finally service. Second milestone could be to build checkout where
  we add this API functionality through each component in the stack from service, then
  daemon (because daemon depends on service), then cli.

- Only describe outcomes in each milestone and observable capalities, e.g., after example milestone 2
  sub-milestone 1, workflow service can serve raw GRPC requests to create workspaces. Detailed APIs,
  technology decisions will be decided when designing each sub-milestone.
