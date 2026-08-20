package ui

import (
	sschema "github.com/bakhod1r/synth/schema"

	"github.com/bakhod1r/seedora/internal/model"
	"github.com/bakhod1r/seedora/internal/plan"
)

// Generator is one entry in the picker the UI shows for a column.
type Generator struct {
	ID    string `json:"id"`
	Label string `json:"label"`
	Group string `json:"group"`
	// Classes are the column classes this generator is sensible for. The UI
	// puts the matching ones first rather than hiding the rest, because a
	// deliberate mismatch — a slug in an integer column's text sibling — is a
	// legitimate choice and hiding it would just be obstruction.
	Classes []model.Class `json:"classes"`
	// Options names the fields of ColumnPlan this generator reads, so the UI
	// renders only the inputs that do something.
	Options []string `json:"options,omitempty"`
}

var (
	text  = []model.Class{model.ClassString}
	num   = []model.Class{model.ClassInt, model.ClassFloat}
	anyOf = []model.Class{
		model.ClassString, model.ClassInt, model.ClassFloat, model.ClassBool,
		model.ClassTime, model.ClassUUID, model.ClassJSON, model.ClassBytes,
		model.ClassEnum, model.ClassUnknown,
	}
)

// Generators is the catalogue the UI offers. It is a curated subset of Synth's
// kinds plus Seedora's own: Synth ships well over a hundred, and a picker with
// every one of them is a worse tool than a picker with the sixty a database
// schema actually calls for.
func Generators() []Generator {
	g := []Generator{
		// Seedora's own come first: they are the ones a schema forces on you.
		{plan.GenForeignKey, "Foreign key", "structure", anyOf, []string{"references"}},
		{plan.GenDefault, "Database default", "structure", anyOf, nil},
		{plan.GenSequence, "Sequential counter", "structure", num, []string{"start"}},
		{plan.GenPattern, "Regex pattern", "structure", text, []string{"pattern"}},
		{plan.GenConst, "Fixed value", "structure", anyOf, []string{"const"}},
		{plan.GenNull, "Always null", "structure", anyOf, nil},

		{k(sschema.KindName), "Full name", "person", text, []string{"locale"}},
		{k(sschema.KindFirstName), "First name", "person", text, []string{"locale"}},
		{k(sschema.KindLastName), "Last name", "person", text, []string{"locale"}},
		{k(sschema.KindMiddleName), "Middle name", "person", text, []string{"locale"}},
		{k(sschema.KindNameSuffix), "Name suffix", "person", text, nil},
		{k(sschema.KindGender), "Gender", "person", text, nil},
		{k(sschema.KindUsername), "Username", "person", text, []string{"unique"}},
		{k(sschema.KindEmail), "Email", "person", text, []string{"unique", "locale"}},
		{k(sschema.KindPhone), "Phone number", "person", text, []string{"locale"}},
		{k(sschema.KindPassword), "Password", "person", text, nil},
		{k(sschema.KindSSN), "National ID", "person", text, []string{"unique"}},
		{k(sschema.KindPassport), "Passport number", "person", text, []string{"unique"}},
		{k(sschema.KindMaritalStatus), "Marital status", "person", text, nil},
		{k(sschema.KindEducation), "Education level", "person", text, nil},
		{k(sschema.KindBloodType), "Blood type", "person", text, nil},

		{k(sschema.KindStreet), "Street address", "address", text, []string{"locale"}},
		{k(sschema.KindCity), "City", "address", text, []string{"locale"}},
		{k(sschema.KindRegion), "Region / state", "address", text, []string{"locale"}},
		{k(sschema.KindPostcode), "Postcode", "address", text, []string{"locale"}},
		{k(sschema.KindCountry), "Country", "address", text, []string{"locale"}},
		{k(sschema.KindCountryCode), "Country code", "address", text, nil},
		{k(sschema.KindContinent), "Continent", "address", text, nil},
		{k(sschema.KindTimezone), "Timezone", "address", text, nil},
		{k(sschema.KindLatitude), "Latitude", "address", num, nil},
		{k(sschema.KindLongitude), "Longitude", "address", num, nil},

		{k(sschema.KindCompany), "Company name", "business", text, []string{"locale"}},
		{k(sschema.KindJob), "Job title", "business", text, nil},
		{k(sschema.KindDepartment), "Department", "business", text, nil},
		{k(sschema.KindJobArea), "Job area", "business", text, nil},
		{k(sschema.KindJobLevel), "Job level", "business", text, nil},
		{k(sschema.KindCatchPhrase), "Catchphrase", "business", text, nil},
		{k(sschema.KindBrand), "Brand", "business", text, nil},

		{k(sschema.KindAmount), "Money amount", "finance", num, []string{"min", "max"}},
		{k(sschema.KindCurrency), "Currency code", "finance", text, nil},
		{k(sschema.KindCurrencyName), "Currency name", "finance", text, nil},
		{k(sschema.KindCurrencySymbol), "Currency symbol", "finance", text, nil},
		{k(sschema.KindIBAN), "IBAN", "finance", text, []string{"unique"}},
		{k(sschema.KindSwift), "SWIFT / BIC", "finance", text, nil},
		{k(sschema.KindCard), "Card number", "finance", text, []string{"unique"}},
		{k(sschema.KindBankName), "Bank name", "finance", text, nil},
		{k(sschema.KindAccountType), "Account type", "finance", text, nil},
		{k(sschema.KindPaymentMethod), "Payment method", "finance", text, nil},
		{k(sschema.KindStockTicker), "Stock ticker", "finance", text, nil},
		{k(sschema.KindCrypto), "Crypto asset", "finance", text, nil},

		{k(sschema.KindProduct), "Product name", "commerce", text, nil},
		{k(sschema.KindProductCategory), "Product category", "commerce", text, nil},
		{k(sschema.KindProductMaterial), "Product material", "commerce", text, nil},
		{k(sschema.KindEAN13), "EAN-13 barcode", "commerce", text, []string{"unique"}},
		{k(sschema.KindISBN), "ISBN", "commerce", text, []string{"unique"}},
		{k(sschema.KindColor), "Colour name", "commerce", text, nil},
		{k(sschema.KindHexColor), "Hex colour", "commerce", text, nil},

		{k(sschema.KindURL), "URL", "internet", text, nil},
		{k(sschema.KindDomain), "Domain", "internet", text, nil},
		{k(sschema.KindSlug), "Slug", "internet", text, []string{"unique"}},
		{k(sschema.KindIPv4), "IPv4 address", "internet", text, nil},
		{k(sschema.KindIPv6), "IPv6 address", "internet", text, nil},
		{k(sschema.KindMAC), "MAC address", "internet", text, nil},
		{k(sschema.KindUserAgent), "User agent", "internet", text, nil},
		{k(sschema.KindHTTPMethod), "HTTP method", "internet", text, nil},
		{k(sschema.KindHTTPStatus), "HTTP status", "internet", []model.Class{model.ClassInt, model.ClassString}, nil},
		{k(sschema.KindMimeType), "MIME type", "internet", text, nil},
		{k(sschema.KindImageURL), "Image URL", "internet", text, nil},
		{k(sschema.KindOS), "Operating system", "internet", text, nil},
		{k(sschema.KindBrowser), "Browser", "internet", text, nil},
		{k(sschema.KindDevice), "Device", "internet", text, nil},
		{k(sschema.KindSemver), "Semantic version", "internet", text, nil},
		{k(sschema.KindFileExt), "File extension", "internet", text, nil},
		{k(sschema.KindMD5), "MD5 hash", "internet", text, []string{"unique"}},
		{k(sschema.KindSHA256), "SHA-256 hash", "internet", text, []string{"unique"}},

		{k(sschema.KindWord), "Single word", "text", text, []string{"max"}},
		{k(sschema.KindSentence), "Sentence", "text", text, []string{"max"}},
		{k(sschema.KindParagraph), "Paragraph", "text", text, []string{"max"}},
		{k(sschema.KindLorem), "Lorem ipsum", "text", text, []string{"max"}},
		{k(sschema.KindTitle), "Title", "text", text, nil},

		{k(sschema.KindInt), "Integer", "primitive", num, []string{"min", "max", "unique"}},
		{k(sschema.KindFloat), "Decimal", "primitive", num, []string{"min", "max"}},
		{k(sschema.KindBool), "Boolean", "primitive", []model.Class{model.ClassBool}, []string{"true_weight"}},
		{k(sschema.KindUUID), "UUID", "primitive", []model.Class{model.ClassUUID, model.ClassString}, []string{"unique"}},
		{k(sschema.KindEnum), "Pick from list", "primitive", anyOf, []string{"values", "weights"}},

		{k(sschema.KindTime), "Timestamp", "time", []model.Class{model.ClassTime}, []string{"min", "max"}},
		{k(sschema.KindUnixTime), "Unix timestamp", "time", num, []string{"min", "max"}},
		{k(sschema.KindYear), "Year", "time", num, []string{"min", "max"}},
		{k(sschema.KindMonth), "Month name", "time", text, nil},
		{k(sschema.KindWeekday), "Weekday name", "time", text, nil},

		{k(sschema.KindVehicleMake), "Vehicle make", "misc", text, nil},
		{k(sschema.KindVehicleModel), "Vehicle model", "misc", text, nil},
		{k(sschema.KindVIN), "VIN", "misc", text, []string{"unique"}},
		{k(sschema.KindLicensePlate), "Licence plate", "misc", text, []string{"unique"}},
		{k(sschema.KindAirport), "Airport", "misc", text, nil},
		{k(sschema.KindAirline), "Airline", "misc", text, nil},
		{k(sschema.KindUniversity), "University", "misc", text, nil},
		{k(sschema.KindLanguageName), "Language", "misc", text, nil},
		{k(sschema.KindFood), "Food", "misc", text, nil},
		{k(sschema.KindDrink), "Drink", "misc", text, nil},
		{k(sschema.KindAnimal), "Animal", "misc", text, nil},
		{k(sschema.KindSport), "Sport", "misc", text, nil},
		{k(sschema.KindMusicGenre), "Music genre", "misc", text, nil},
		{k(sschema.KindFramework), "Framework", "misc", text, nil},
		{k(sschema.KindEmoji), "Emoji", "misc", text, nil},
	}
	// Every generator can be nulled or made unique; the UI shows those as
	// checkboxes rather than as options here.
	return g
}

func k(kind sschema.Kind) string { return string(kind) }
