---
title: "Federation"
description: "Log in through OIDC, LDAP, or client certificates."
weight: 30
---

liteio can issue temporary credentials to identities that already live in an
external system, so you do not have to mint a liteio user for every person or
service. Register a provider through the admin API, and the STS endpoint handles
the exchange.

## OIDC

Let anyone in an OIDC provider (Keycloak, Auth0, Okta, Google) assume a role and
get S3 access.

Register the provider:

```bash
curl -X POST http://localhost:9001/minio/v1/idp/openid \
  -u admin:changeme \
  -d '{
    "name": "keycloak",
    "configURL": "https://keycloak.example.com/realms/myrealm/.well-known/openid-configuration",
    "clientID": "liteio",
    "clientSecret": "...",
    "rolePolicy": "readonly",
    "claimName": "roles",
    "claimValue": "s3-users"
  }'
```

Trade a JWT for credentials:

```bash
curl -X POST "http://localhost:9000/?Action=AssumeRoleWithWebIdentity&Version=2011-06-15" \
  -d "WebIdentityToken=$JWT&DurationSeconds=3600"
```

```xml
<AssumeRoleWithWebIdentityResponse>
  <AssumeRoleWithWebIdentityResult>
    <Credentials>
      <AccessKeyId>...</AccessKeyId>
      <SecretAccessKey>...</SecretAccessKey>
      <SessionToken>...</SessionToken>
      <Expiration>2026-06-10T13:00:00Z</Expiration>
    </Credentials>
  </AssumeRoleWithWebIdentityResult>
</AssumeRoleWithWebIdentityResponse>
```

Sign subsequent S3 requests with those three values, and they expire on their
own at the time shown.

## LDAP

Bind against a directory to authenticate users and turn their group membership
into a policy.

Register the directory:

```bash
curl -X POST http://localhost:9001/minio/v1/idp/ldap \
  -u admin:changeme \
  -d '{
    "serverAddr": "ldap://ldap.example.com:389",
    "bindDN": "cn=readonly,dc=example,dc=com",
    "bindPassword": "...",
    "userDNSearchFilter": "(uid=%s)",
    "userDNSearchBase": "ou=users,dc=example,dc=com",
    "groupSearchFilter": "(member=%s)",
    "groupSearchBase": "ou=groups,dc=example,dc=com",
    "groupNameAttr": "cn"
  }'
```

Assume a role with a username and password:

```bash
curl -X POST "http://localhost:9000/?Action=AssumeRoleWithLDAPIdentity&Version=2011-06-15" \
  -d "LDAPUsername=alice&LDAPPassword=alicepw&DurationSeconds=3600"
```

The session's policy is the union of the policies attached to every group the
user belongs to.

## Client certificates

A client holding an X.509 certificate signed by a trusted CA can assume a role
with no password at all.

Register the CA:

```bash
curl -X POST http://localhost:9001/minio/v1/idp/cert \
  -u admin:changeme \
  -d '{
    "caCert": "-----BEGIN CERTIFICATE-----\n...",
    "subjectClaim": "CN",
    "rolePolicy": "readonly"
  }'
```

Present the certificate in the TLS handshake:

```bash
curl -X POST "https://localhost:9000/?Action=AssumeRoleWithCertificate&Version=2011-06-15" \
  --cert client.crt --key client.key --cacert cluster-ca.crt
```

liteio validates the chain, reads the subject field named in `subjectClaim`, and
maps it to the role's policy.
