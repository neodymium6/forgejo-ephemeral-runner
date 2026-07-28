# Project Guidance

This repository is intended for eventual public release. Keep all source,
examples, tests, and documentation environment-neutral.

## Safety and privacy

- Never commit credentials, runner tokens, private keys, real internal domains,
  private IP addresses, cluster identifiers, hardware identifiers, or personal
  contact details.
- Use `example.com`, `example.invalid`, and clearly fake values in examples.
- Keep real deployment overlays and encrypted secrets in the operator's private
  infrastructure repository, not here.
- Do not apply manifests to a live cluster without explicit approval for that
  cluster and revision.

## Development

- Preserve the one-job, one-Pod security boundary.
- Do not mount host paths, container runtime sockets, or service account tokens
  into the workflow container.
- Keep the Forgejo registration token exclusive to the registration init
  container.
- Run `just check` before committing.
- Write code, comments, commits, and documentation in English.
