# Omnigrex

Omnigrex is a platform for coordinating autonomous software-development agents while keeping GitHub as the human-facing source of truth.

The project is currently defining and implementing its first dogfooding iteration:

```text
Human -> GitHub Issue -> Developer -> Pull Request -> Reviewer -> Human
```

## Documentation

- [Project concept](./project-concept.md)
- [First-iteration specification](./docs/first-iteration.md)
- [Implementation plan](./docs/implementation-plan.md)
- [Domain language](./CONTEXT.md)
- [Architecture decisions](./docs/adr/)

The implementation will be written in Go and deployable with Docker Compose.
