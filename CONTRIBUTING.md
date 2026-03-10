# Contributing to Kernel

First off, thank you for considering contributing to Kernel! It's people like you that make Kernel such a great tool.

## Code of Conduct

By participating in this project, you are expected to uphold our [Code of Conduct](./CODE_OF_CONDUCT.md). Please report unacceptable behavior to <oss@kernel.sh>.

## Getting Started

## Development Workflow

1. **Fork the repository**
   - Fork the repository on GitHub to your personal account
   - Clone your fork locally: `git clone https://github.com/YOUR_USERNAME/kernel-images.git`
   - Add upstream remote: `git remote add upstream https://github.com/onkernel/kernel-images.git`

2. **Create a new branch**
   - Always branch from the up-to-date main branch
   ```bash
   git checkout main
   git pull upstream main
   git checkout -b feature/your-feature-name
   ```

3. **Make your changes**
   - Write your code following our coding standards
   - Ensure all tests pass: `npm test`
   - Add tests for new functionality

4. **Commit your changes**
   - Use clear and meaningful commit messages
   - Follow conventional commits format: `type(scope): message`
   - Example: `feat(api): add new endpoint for user authentication`

5. **Submit a pull request**
   - Push to your fork: `git push origin feature/your-feature-name`
   - Create a pull request from your branch to our main branch
   - Fill out the PR template completely

## Pull Request Process

1. Update the README.md or documentation with details of changes if applicable
2. The PR requires approval from at least one maintainer
3. You may merge the PR once it has the required number of approvals

## Coding Standards

- **Linting**: We use ESLint to enforce coding standards
  ```bash
  npm run lint
  ```
- **Formatting**: We use Prettier for code formatting
  ```bash
  npm run format
  ```
- All PRs must pass our linting and formatting checks to be merged

## Testing

- Write unit tests for all new functionality
- Ensure all tests pass before submitting a PR
- Aim for at least 80% code coverage for new code

## Documentation

- Update documentation for any new or changed functionality
- Document TypeScript interfaces, classes, and methods
- Keep our guides and tutorials up to date

## Reporting Issues

- Use the GitHub issue tracker to report bugs
- Check existing issues before opening a new one
- Fill out the issue template completely
- Include steps to reproduce, expected behavior, and actual behavior

## Licensing

- By contributing to Kernel, you agree that your contributions will be licensed under the project's license
- For questions about licensing, please contact the project maintainers or <oss@kernel.sh>

## Communication

- GitHub Issues: Bug reports, feature requests, and discussions
- [Discord](https://discord.gg/FBrveQRcud): For general questions and community discussions
- Email: For security concerns or Code of Conduct violations

## Recognition

Contributors who make significant improvements may be invited to join as project maintainers.

Thank you for contributing to Kernel! ❤️
