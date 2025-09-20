### JSON file

**vocative.json** contains about 1800 known names with vocative form in Serbia and Balkans.

# Deklinacija API

Free for commercial use. There is no limit on usage or pricing, but please be respectful and don't abuse the API.

Slobodno korišćenje API-a za komercijalne svrhe. Nema ograničenja niti cene, ali vas molim da budete pošteni i da ne zloupotrebljavate API.

## Usage

GET `https://deklinacija.com/api/:name` (case-insensitive)

Example request:
```http
GET /api/Ognjen
```

200 Success response:
```json
{
  "name": "Ognjen",
  "sex": "male",
  "vocative": "Ognjene",
  "status": "Success"
}
```

404 Not Found response:
```json
{
  "name": "Ognjan",
  "sex": "",
  "vocative": "",
  "status": "Not found"
}
```
