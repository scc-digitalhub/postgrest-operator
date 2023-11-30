# postgrest-operator

Currently, the operator will only be able to create PostgREST instances to expose data from the database the operator is configured for.

## Installation
There is an available deployment file ready to be used. Install operator and CRD:
```sh
kubectl apply -f deployment.yaml
```

An example CR is found at `config/samples/operator_v1_postgrest.yaml`. The CRD included in the deployment file is found at `config/crd/bases/operator.postgrest.org_postgrests.yaml`.

Launch CR:
```sh
kubectl apply -f config/samples/operator_v1_postgrest.yaml
```

## PostgREST custom resource
A PostgREST custom resource's properties are:

- `schema`: **Required**. The schema PostgREST will expose.
- `anonRole`: *Optional*. The role PostgREST will use to authenticate. If specified, it is assumed to already exist and already have the intended permissions on tables. If not specified, will be auto-generated as as `<CR name>_postgrest_role`.
- `tables`: *Optional*. Do not set if you already set `anonRole`. List of tables within the schema to expose. If left empty with an auto-generated role, effectively nothing will be exposed.
- `grants`: *Optional*. Ignored if you already set `anonRole`. Comma-separated string listing actions permitted on tables. Defaults to `SELECT` if not specified. A "full" string is `INSERT, SELECT, UPDATE, DELETE, TRUNCATE, REFERENCES, TRIGGER`, but you may also use `ALL`.

A valid sample spec configuration is:
``` yaml
...
spec:
  schema: operator
  tables:
    - test
  grants: SELECT, UPDATE, INSERT, DELETE
```

Another valid sample:
``` yaml
...
spec:
  schema: operator
  anonRole: anon
```
