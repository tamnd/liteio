---
title: "Federation"
description: "OIDC, LDAP, and certificate-based identity federation via STS."
weight: 30
---

liteio can issue STS temporary credentials to identities authenticated by an
external provider. Configure one or more identity providers in the admin API;
the STS endpoint handles the exchange.

## OIDC / web identity

Allow users from any OIDC provider (Keycloak, Auth0, Okta, Google, ...) to
assume a role and get S3 access.

### Configure the OIDC provider

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

### Exchange a JWT for STS credentials

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

Use those three values (access key, secret key, session token) to sign S3
requests as the federated identity.

## LDAP

Bind against an LDAP directory to authenticate users and map group membership
to IAM policies.

### Configure LDAP

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

### Assume role with LDAP

```bash
curl -X POST "http://localhost:9000/?Action=AssumeRoleWithLDAPIdentity&Version=2011-06-15" \
  -d "LDAPUsername=alice&LDAPPassword=alicepw&DurationSeconds=3600"
```

The policy for the session is the union of IAM policies attached to any group
the user belongs to.

## Certificate federation

Clients with X.509 certificates signed by a trusted CA can assume a role without
a password.

### Configure the certificate CA

```bash
curl -X POST http://localhost:9001/minio/v1/idp/cert \
  -u admin:changeme \
  -d '{
    "caCert": "-----BEGIN CERTIFICATE-----\n...",
    "subjectClaim": "CN",
    "rolePolicy": "readonly"
  }'
```

### Assume role with certificate

Present the certificate in the TLS handshake:

```bash
curl -X POST "https://localhost:9000/?Action=AssumeRoleWithCertificate&Version=2011-06-15" \
  --cert client.crt --key client.key --cacert cluster-ca.crt
```
