# PostgREST operator

A Kubernetes operator to start instances of PostgREST.

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
- `connection`: **Required**. A structure to indicate the database to connect to. Its sub-properties are:
  - `host`: **Required**.
  - `port`: *Optional*.
  - `database`: **Required**.
  - `user`: Used with `password` to initialize PostgREST. Do not provide if `secretName` is provided.
  - `password`: Used with `user` to initialize PostgREST. Do not provide if `secretName` is provided.
  - `extraParams`: *Optional*. String for extra connection parameters, in the format `parameter1=value&parameter2=value`.
  - `secretName`: Name of a Kubernetes secret containing connection properties. Do not provide if `user` and `password` are provided. More information in a later section.
 
Note that you must provide either `secretName`, or `user` and `password`, but if you provide the former, do not provide the latter two, and vice versa.
 
A valid sample spec configuration is:
``` yaml
...
spec:
  schema: operator
  anonRole: anon
  connection:
    host: 192.168.123.123
    database: postgres
    user: postgres
    password: postgres
```

Another valid sample:
``` yaml
...
spec:
  schema: operator
  tables:
    - test
  grants: SELECT, UPDATE, INSERT, DELETE
  connection:
    host: 192.168.123.123
    port: 5432
    database: postgres
    extraParams: sslmode=disable
    secretName: mysecret
```

## Using a K8S secret to authenticate

Instead of writing user and password as properties, you can provide a `connection.secretName` property, containing a string with the name of a Kubernetes secret to use to authenticate.

Here is a sample file you can apply with `kubectl apply -f secret-file.yml` to create the secret:
``` yaml
apiVersion: v1
kind: Secret
metadata:
  name: mysecret
  namespace: postgrest-operator-system
stringData:
  POSTGRES_URL: postgresql://postgres:postgres@192.168.123.123:5432/postgres?sslmode=disable
  USER: postgres # Only required if POSTGRES_URL is not provided
  PASSWORD: postgres # Only required if POSTGRES_URL is not provided
```
If you omit `POSTGRES_URL`, then `USER` and `PASSWORD` are required, but if you provide it, they will be ignored.

`POSTGRES_URL` uses the format `postgresql://user:password@host:port/database?parameter1=value&parameter2=value` and *host* and *database* must match the values defined in the CR's `connection` properties.
