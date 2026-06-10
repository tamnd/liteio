---
title: "Security"
linkTitle: "Security"
description: "Authentication, IAM, federation, and encryption."
weight: 40
featured: true
---

liteio implements the AWS security model end to end: SigV4 on every request,
IAM policy evaluation with deny-by-default, STS for short-lived credentials,
federated login through OIDC, LDAP, or client certificates, and per-object
encryption with customer-held keys.
