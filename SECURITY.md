# Security

protonfs has access to your Proton session and decrypted file contents, so security reports are taken seriously.

**Please don't open a public issue for vulnerabilities.** Report them privately through GitHub: on the repository's **Security** tab, choose **Report a vulnerability**.

Examples of what to report:

- session tokens, key passwords or decrypted data leaking into logs, world-readable files or other processes
- other local users being able to read the mount or the cache
- the installer running or installing something it hasn't verified

Only the latest release is supported.

protonfs is not affiliated with Proton AG. Report vulnerabilities in Proton's own services, SDK or CLI to [Proton's bug bounty program](https://proton.me/security/bug-bounty).
