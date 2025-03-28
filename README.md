# VSC Node (Golang Rewrite)

The VSC node is the core of the VSC ecosystem, holding the code necessary for keeping the VSC network operational.

## Getting Involved

- If you are interested in operating the VSC node yourself and contributing to our growing network, explore the [deployment repository](https://github.com/vsc-eco/vsc-deployment) for all the details you need to get started.
- Join our [Discord server](https://discord.gg/F5Eqh2XYuY) to get in touch with the VSC community and ask any questions you might have.

## GraphQL API

To regenerate the GraphQL code, run:

```bash
go run github.com/99designs/gqlgen generate
```

To view the GraphQL playground, start the `gql` package and navigate to whatever URL specified when creating a new `gql` with `gql.New(...)` + `/sandbox`.