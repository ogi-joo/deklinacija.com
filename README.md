### JSON file

**vocative.json** contains about 1800 known names with vocative form in Serbia and Balkans.

# Deklinacija API

[deklinacija.com](https://deklinacija.com)

Free commercial use. There is no limit on usage or pricing, but please be respectful and don't abuse the API.

Slobodno korišćenje API-a za komercijalne svrhe. Nema ograničenja niti cene, ali vas molim da budete pošteni i da ne zloupotrebljavate API.

## Usage

GET `https://deklinacija.com/api/:name` (case-insensitive)

Supports Serbian Cyrillic input. For example, `/api/Огњен` is equivalent to `/api/Ognjen`.

Example request:
```http
GET /api/Ognjen HTTP/1.1
Host: localhost:3009
```

200 Success response:
```json
{
  "name": "Ognjen",
  "sex": "male",
  "vocative": "Ognjene",
  "vocative_cyr": "Огњене",
  "status": "Success"
}
```

404 Not Found response:
```json
{
  "name": "Ognjan",
  "sex": null,
  "vocative": null,
  "vocative_cyr": null,
  "status": "Not found"
}
```

## Response format

```typescript
{
  "name": string,
  "sex": "male" | "female" | "both" | null,
  "vocative": string | null,
  "vocative_cyr": string | null,
  "status": "Success" | "Not found"
}
```


