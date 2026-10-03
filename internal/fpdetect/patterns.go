// Fichier des tables de motifs linguistiques du détecteur de fingerprinting.
//
// COMPARAISON PAR SOUS-CHAÎNE. Toutes les listes ci-dessous sont destinées à être
// comparées par sous-chaîne (strings.Contains) sur un texte préalablement mis en
// minuscules (strings.ToLower), via hitContains (detector.go) pour les listes de
// phrases, ou directement pour les listes techniques. Elles sont donc toutes
// écrites en minuscules.
//
// POURQUOI AUCUNE EXPRESSION RÉGULIÈRE, AUCUN \b. Les frontières de mot (\b) sont
// inopérantes pour les langues sans séparateurs de mots — chinois, japonais,
// coréen — et peu fiables pour l'arabe ou l'hindi. Un motif `what model` ancré par
// \b raterait « 你是什么模型 », qui n'a aucun espace. La sous-chaîne littérale est
// en plus prévisible, directement testable, et moins chère qu'un moteur regex ;
// les listes techniques (outils, streaming) ne sont pas non plus des motifs de
// texte libre, mais des noms de champs et des fragments de noms d'outils.
//
// DIACRITIQUES. Le texte est comparé tel quel après passage en minuscules, sans
// normalisation Unicode : « quel modèle » et « quel modele » sont deux motifs
// distincts, et les deux sont nécessaires. Chaque formulation accentuée est donc
// accompagnée de sa forme sans accent quand la langue en utilise.
//
// ORGANISATION. Les motifs sont regroupés par langue, avec un commentaire au-dessus
// de chaque groupe. Aucun doublon, aucune chaîne vide, aucun motif d'un seul
// caractère (le détecteur ignore de toute façon les chaînes vides).

package fpdetect

import "strings"

// modelIdentityPhrases : formulations qui demandent explicitement l'identité ou le
// nom du modèle qui répond. Signal FORT (SigModelQuestion) : une question d'identité
// nue n'apparaît pas dans un usage applicatif normal.
//
// « identifiez-vous » (fr) et « identifique-se » (pt) sont volontairement exclus :
// ce sont d'abord les libellés d'interface de « se connecter », trop ambigus pour
// un signal qui route à lui seul.
var modelIdentityPhrases = []string{
	// --- Anglais ---
	"are you actually claude",
	"are you claude",
	"are you gpt",
	"are you really claude",
	"are you a real claude",
	"identify yourself",
	"what ai are you",
	// Ajouts du 2026-10-02 : « Which AI model are you exactly? Which company
	// trained you? » n'a été détecté par AUCUN motif — la question d'identité la plus
	// ordinaire en anglais passait donc au parc, alors qu'elle doit partir chez le
	// fournisseur externe. Aucune de ces formes ne décrit une question légitime sur un
	// produit : elles portent toutes sur l'IDENTITÉ du modèle qui répond.
	"what ai model are you",
	"what company trained you",
	"what model do you use",
	"what model are you using",
	"what model is answering",
	"what's your model",
	"whats your model",
	"which ai are you",
	"which ai model are you",
	"which company developed you",
	"which company trained you",
	"which company is behind you",
	"which lab made you",
	"which lab trained you",
	"which model are you using",
	"which model is answering",
	"who is your creator",
	"who is your developer",
	"what llm",
	"what model are you",
	"what model are you running",
	"what model is this",
	"what version of claude",
	"which model are you",
	"who are you",
	// Formes abrégées d'usage courant (2026-10-03) : mêmes questions, écrites vite.
	"who are u",
	"what are u",
	"what model r u",
	"which model r u",

	// --- Français ---
	// Formes PARLÉES ajoutées le 2026-10-03 après un vrai test client : « Tu es qui ? »
	// n'était reconnue par aucun motif (la table ne contenait que l'inversion
	// « qui es-tu ») et la question est donc restée sur le parc, sans consommation chez
	// le fournisseur externe. Un client qui teste écrit comme il parle : les deux formes
	// doivent être présentes.
	"c'est quel modele",
	"c'est quel modèle",
	"c'est quoi comme modele",
	"c'est quoi comme modèle",
	"c'est quoi ton modele",
	"c'est quoi ton modèle",
	"c'est qui toi",
	"c'est qui vous",
	"es-tu claude",
	"es-tu gpt",
	"identifie-toi",
	"quel est ton modele",
	"quel est ton modèle",
	"quel llm",
	"quel modele es-tu",
	"quel modèle es-tu",
	"quelle version de claude",
	"qui es-tu",
	"qui etes-vous",
	"qui êtes-vous",
	"qui est tu",
	"t'es qui",
	"t'es quoi",
	"tes qui",
	"tu es claude",
	"tu es gpt",
	"tu es quel modele",
	"tu es quel modèle",
	"tu es quelle ia",
	"tu es qui",
	"tu es quoi",
	"tu tournes sur quel modele",
	"tu tournes sur quel modèle",
	"tu utilises quel modele",
	"tu utilises quel modèle",
	"tu es base sur quel modele",
	"tu es basé sur quel modèle",
	"votre modele est quoi",
	"votre modèle est quoi",
	"vous etes claude",
	"vous etes quel modele",
	"vous etes qui",
	"vous êtes claude",
	"vous êtes quel modèle",
	"vous êtes qui",

	// --- Chinois simplifié ---
	"什么模型",
	"你是什么大模型",
	"你是什么模型",
	"你是什么版本的claude",
	"你是哪个模型",
	"你是claude吗",
	"你是gpt吗",
	"你是谁",
	"你用的什么模型",
	"哪个模型",

	// --- Chinois traditionnel ---
	"什麼模型",
	"你是什麼大模型",
	"你是什麼模型",
	"你是什麼版本的claude",
	"你是哪個模型",
	"你是claude嗎",
	"你是gpt嗎",
	"你是誰",
	"你用什麼模型",
	"哪個模型",

	// --- Espagnol ---
	"cual es tu modelo",
	"cuál es tu modelo",
	"eres claude",
	"eres gpt",
	"identificate",
	"identifícate",
	"que llm eres",
	"que modelo eres",
	"que modelo es este",
	"que version de claude",
	"qué llm eres",
	"qué modelo eres",
	"qué modelo es este",
	"qué versión de claude",
	"quien eres",
	"quien eres tú",
	"quién eres",

	// --- Portugais ---
	"qual e o seu modelo",
	"qual versao do claude",
	"qual é o seu modelo",
	"qual versão do claude",
	"que llm voce e",
	"que llm você é",
	"que modelo e este",
	"que modelo voce e",
	"que modelo você é",
	"que modelo é este",
	"quem e voce",
	"quem é você",
	"voce e claude",
	"voce e gpt",
	"você é claude",
	"você é gpt",

	// --- Allemand ---
	"bist du claude",
	"bist du gpt",
	"identifiziere dich",
	"welche version von claude",
	"welches llm bist du",
	"welches modell antwortet",
	"welches modell bist du",
	"welches modell ist das",
	"welches modell sind sie",
	"wer bist du",
	"wer sind sie",

	// --- Italien ---
	"che modello sei",
	"chi sei",
	"identificati",
	"quale llm sei",
	"quale modello e questo",
	"quale modello sei",
	"quale modello è questo",
	"quale versione di claude",
	"sei claude",
	"sei gpt",

	// --- Russe ---
	"какая версия claude",
	"какая модель",
	"какая модель отвечает",
	"какая ты модель",
	"какая у тебя модель",
	"кто ты",
	"назови себя",
	"ты claude",
	"ты gpt",
	"что ты за модель",

	// Ajouts mesurés en production le 2026-09-29 : « Who trained you? » a été servi par
	// WorkBuddy, qui a répondu « I was trained by Z.ai. » — le vrai fournisseur du modèle,
	// nommé au client qui avait demandé claude-opus-5. Les tournures « qui t'a fait /
	// entraîné / développé » manquaient : la table ne couvrait que la DATE d'entraînement
	// (« when were you trained »), pas l'AUTEUR. Formes volontairement non ambiguës
	// (aucune ne décrit une question légitime sur un produit).
	"which company built you",
	"which company created you",
	"which company made you",
	"who built you",
	"who created you",
	"who developed you",
	"who is behind you",
	"who made you",
	"who owns you",
	"who trained you",
	"quelle entreprise t'a créé",
	"quelle entreprise t'a développé",
	"qui t'a créé",
	"qui t'a développé",
	"qui t'a entrainé",
	"qui t'a entraîné",
	"qui vous a créé",
	"qui vous a développé",

	// --- Arabe ---
	// Les mêmes tournures sont doublées sans hamza : au clavier arabe, « أنت » s'écrit
	// presque toujours « انت » et « أي » s'écrit « اي ».
	"أي نموذج أنت",
	"اي نموذج انت",
	"عرّف بنفسك",
	"عرف بنفسك",
	"ما إصدار claude",
	"ما اصدار claude",
	"ما اسم النموذج",
	"ما هو llm",
	"ما هو النموذج",
	"ما هو النموذج الذي يرد",
	"من أنت",
	"من انت",
	"هل أنت claude",
	"هل أنت gpt",
	"هل أنت كلود",
	"هل انت claude",
	"هل انت gpt",
	"هل انت كلود",

	// --- Hindi ---
	"अपनी पहचान बताओ",
	"आप कौन हैं",
	"आप कौन सा मॉडल हैं",
	"कौन सा llm",
	"कौन सा मॉडल है",
	"कौन सा मॉडल हो",
	"क्या आप gpt हैं",
	"क्या तुम claude हो",
	"क्या तुम क्लॉड हो",
	"तुम कौन हो",
	"तुम कौन सा मॉडल हो",
	"तुम्हारा मॉडल क्या है",

	// --- Japonais ---
	"claudeですか",
	"claudeのどのバージョン",
	"gptですか",
	"あなたは何のモデルですか",
	"あなたは誰ですか",
	"あなたのモデルは何ですか",
	"どのモデル",
	"どのモデルですか",
	"どのモデルを使っていますか",
	"何のモデル",

	// --- Coréen ---
	"claude 어떤 버전이야",
	"claude야",
	"gpt야",
	"너는 누구야",
	"너는 무슨 모델이야",
	"모델 이름이 뭐야",
	"무슨 모델이야",
	"어떤 llm이야",
	"어떤 모델을 사용해",
	"어떤 모델이야",

	// --- Turc ---
	"claude hangi surum",
	"claude hangi sürüm",
	"claude musun",
	"gpt misin",
	"hangi llm",
	// La forme longue précède la forme courte : firstIdentityHit (detector.go) retient
	// le premier motif trouvé, et « hangi model » — ambigu, donc réservé aux requêtes
	// brèves — masquerait sinon « hangi model cevap veriyor » (25 runes).
	"hangi model cevap veriyor",
	"hangi model",
	"hangi modelsin",
	"kendini tanit",
	"kendini tanıt",
	"modelin ne",
	"sen kimsin",

	// --- Néerlandais ---
	"ben je claude",
	"ben je gpt",
	"identificeer jezelf",
	"welk model antwoordt",
	"welk model ben je",
	"welk model ben jij",
	"welk model bent u",
	"welk model is dit",
	"welke llm ben je",
	"welke versie van claude",
	"wie ben je",

	// --- Polonais ---
	"czy jestes claude",
	"czy jestes gpt",
	"czy jesteś claude",
	"czy jesteś gpt",
	"jaki llm",
	"jaki to model",
	"jakim modelem jestes",
	"jakim modelem jesteś",
	"kim jestes",
	"kim jesteś",
	"ktora wersja claude",
	"ktory model",
	"który model",
	"podaj nazwe modelu",
	"podaj nazwę modelu",

	// --- Vietnamien ---
	"ban co phai claude khong",
	"ban co phai gpt khong",
	"ban dung mo hinh gi",
	"ban la ai",
	"ban la llm gi",
	"ban la mo hinh gi",
	"bạn có phải claude không",
	"bạn có phải gpt không",
	"bạn dùng mô hình gì",
	"bạn là ai",
	"bạn là llm gì",
	"bạn là mô hình gì",
	"cho toi biet mo hinh",
	"cho tôi biết mô hình",
	"mo hinh nao",
	"mô hình nào",
}

// cutoffPhrases : formulations qui demandent explicitement la date de coupure de
// connaissance du modèle. Signal FORT (SigCutoffExplicit) : même une question
// légitime sur les limites du modèle trahit le plus souvent une sonde d'empreinte.
var cutoffPhrases = []string{
	// --- Anglais ---
	"cutoff date",
	"knowledge cutoff",
	"training cutoff",
	"training data cutoff",
	"up to when are you trained",
	"what date were you trained",
	"what is your cutoff",
	"what is your knowledge cutoff",
	"when does your knowledge end",
	"when were you trained",

	// --- Français ---
	"connaissances jusqu'a",
	"connaissances jusqu'à",
	"coupure de connaissance",
	"coupure des connaissances",
	"date d'entrainement",
	"date d'entraînement",
	"date de coupure",
	"fin de tes connaissances",
	"jusqu'a quand as-tu ete entraine",
	"jusqu'a quand es-tu entraine",
	"jusqu'à quand as-tu été entraîné",
	"jusqu'à quand es-tu entraîné",
	"quelle est ta date de coupure",

	// --- Chinois simplifié ---
	"你的知识截止到什么时候",
	"你的训练数据到什么时候",
	"截止日期",
	"知识截止",
	"知识截止日期",
	"知识更新到",
	"训练截止",
	"训练数据截止",

	// --- Chinois traditionnel ---
	// (« 截止日期 » est identique en simplifié et traditionnel : il n'est listé
	//  qu'une seule fois, dans le groupe simplifié ci-dessus.)
	"你的知識截止到什麼時候",
	"知識截止",
	"知識截止日期",
	"知識更新到",
	"訓練截止",
	"訓練資料截止",

	// --- Espagnol ---
	"conocimiento hasta",
	"corte de conocimiento",
	"corte de entrenamiento",
	"cual es tu fecha de corte",
	"cuál es tu fecha de corte",
	"datos de entrenamiento hasta",
	"fecha de corte",
	"fecha de entrenamiento",
	"hasta cuando fuiste entrenado",
	"hasta cuándo fuiste entrenado",
	"hasta que fecha estas entrenado",
	"hasta qué fecha estás entrenado",

	// --- Portugais ---
	"ate quando voce foi treinado",
	"até quando você foi treinado",
	"conhecimento ate",
	"conhecimento até",
	"corte de conhecimento",
	"corte de treinamento",
	"dados de treinamento ate",
	"dados de treinamento até",
	"data de corte",
	"data de treinamento",
	"qual e a sua data de corte",
	"qual é a sua data de corte",

	// --- Allemand ---
	"bis wann wurden sie trainiert",
	"ende deines wissens",
	"trainingsdaten bis",
	"trainingsstichtag",
	"wann wurdest du trainiert",
	"wie aktuell sind deine daten",
	"wissens cutoff",
	"wissensstand",
	"wissensstichtag",

	// --- Italien ---
	"conoscenze fino a",
	"cutoff delle conoscenze",
	"data di addestramento",
	"data di taglio",
	"dati di addestramento fino a",
	"fino a quando sei stato addestrato",
	"qual e la tua data di taglio",
	"qual è la tua data di taglio",
	"taglio delle conoscenze",

	// --- Russe ---
	"дата обучения",
	"дата отсечения знаний",
	"дата отсечки",
	"до какого года ты обучен",
	"до какого года твои знания",
	"до какого месяца ты обучен",
	"когда заканчиваются твои знания",
	"когда ты был обучен",
	"отсечка знаний",
	"по какое число ты обучен",

	// --- Arabe ---
	"إلى أي تاريخ تدربت",
	"تاريخ التدريب",
	"تاريخ القطع",
	"تاريخ انتهاء المعرفة",
	"تاريخ قطع المعرفة",
	"حتى متى تم تدريبك",
	"معرفتك حتى",
	"ما هو تاريخ قطع معرفتك",

	// --- Hindi ---
	"आपका डेटा कब तक का है",
	"कटऑफ तिथि",
	"ज्ञान कटऑफ",
	"ज्ञान कब तक",
	"तुम्हारा ज्ञान कब तक है",
	"तुम्हें कब तक प्रशिक्षित किया गया",
	"प्रशिक्षण कटऑफ",
	"प्रशिक्षण डेटा कब तक",

	// --- Japonais ---
	"いつまで学習しましたか",
	"いつまでの知識",
	"カットオフ日",
	"学習データのカットオフ",
	"学習データはいつまで",
	"知識カットオフ",
	"知識のカットオフ",
	"訓練データの締め切り",

	// --- Coréen ---
	"언제까지 학습했어",
	"지식 마감",
	"지식 컷오프",
	"지식 컷오프 날짜",
	"컷오프 날짜",
	"훈련 데이터",
	"학습 데이터 컷오프",
	"학습 데이터는 언제까지",

	// --- Turc ---
	"bilgi kesme tarihi",
	"bilgin nereye kadar",
	"egitim kesme tarihi",
	"egitim verisi",
	"eğitim kesme tarihi",
	"eğitim verisi",
	"hangi tarihe kadar egitildin",
	"hangi tarihe kadar eğitildin",
	"kesim tarihi",
	"ne zamana kadar egitildin",
	"ne zamana kadar eğitildin",

	// --- Néerlandais ---
	"afsluitdatum",
	"einde van je kennis",
	"kennis tot",
	"kennisafsluitdatum",
	"tot wanneer ben je getraind",
	"trainingsafsluitdatum",
	"trainingsdata tot",
	"wanneer ben je getraind",

	// --- Polonais ---
	"data odciecia",
	"data odcięcia",
	"data treningu",
	"do kiedy byles trenowany",
	"do kiedy byłeś trenowany",
	"kiedy zostales wytrenowany",
	"kiedy zostałeś wytrenowany",
	"odciecie wiedzy",
	"odcięcie wiedzy",
	"wiedza do",

	// --- Vietnamien ---
	"ban duoc huan luyen den khi nao",
	"bạn được huấn luyện đến khi nào",
	"du lieu huan luyen den",
	"dữ liệu huấn luyện đến",
	"kien thuc den",
	"kiến thức đến",
	"ngay cat kien thuc",
	"ngay ket thuc du lieu",
	"ngày cắt kiến thức",
	"ngày kết thúc dữ liệu",
	"thoi diem cat kien thuc",
	"thời điểm cắt kiến thức",
	"когда заканчивается обучение",
	"до какого числа ты обучен",
}

// recentFactPhrases : formulations qui portent sur des faits très récents.
//
// ATTENTION — SIGNAL FAIBLE (SigRecentFact). Ces formulations sont ambiguës : un vrai
// client peut poser une question parfaitement légitime sur 2026, sur « cette semaine »
// ou sur « aujourd'hui ». La liste reste volontairement littérale et factuelle, sans
// vocabulaire inventé : elle ne sert qu'à corroborer d'autres signaux, jamais à router
// à elle seule. La détection d'année récente (recentYearMention) est testée avant
// cette liste, qui n'est évaluée qu'en son absence.
var recentFactPhrases = []string{
	// --- Anglais ---
	"as of 2026",
	"last month",
	"latest news",
	"recently",
	"these days",
	"this week",
	"this year",
	"today",
	"what happened recently",
	"what happened this year",

	// --- Français ---
	"actualite recente",
	"actualité récente",
	"aujourd'hui",
	"ces derniers jours",
	"cette annee",
	"cette année",
	"cette semaine",
	"le mois dernier",
	"que s'est-il passe recemment",
	"que s'est-il passé récemment",
	"recemment",
	"récemment",

	// --- Chinois simplifié ---
	"上个月",
	"本周",
	"这周",
	"这几天",
	"最近发生了什么",

	// --- Chinois traditionnel ---
	"上個月",
	"本週",
	"這週",
	"這幾天",
	"最近發生了什麼",

	// --- Espagnol ---
	"el mes pasado",
	"este año",
	"estos dias",
	"estos días",
	"hoy",
	"noticias recientes",
	"que paso recientemente",
	"qué pasó recientemente",

	// --- Portugais ---
	"em 2026",
	"hoje",
	"neste ano",
	"no mes passado",
	"no mês passado",
	"noticias recentes",
	"notícias recentes",
	"o que aconteceu recentemente",

	// --- Allemand ---
	"aktuelle nachrichten",
	"diese woche",
	"dieses jahr",
	"heute",
	"im jahr 2026",
	"in letzter zeit",
	"kürzlich",
	"letzten monat",
	"vor kurzem",
	"was ist kürzlich passiert",

	// --- Italien ---
	"cosa e successo di recente",
	"cosa è successo di recente",
	"il mese scorso",
	"in questi giorni",
	"nel 2026",
	"notizie recenti",
	"oggi",
	"quest'anno",
	"questa settimana",

	// --- Russe ---
	"в 2026",
	"в последнее время",
	"в прошлом месяце",
	"в этом году",
	"на этой неделе",
	"недавно",
	"последние новости",
	"сегодня",
	"что произошло недавно",

	// --- Arabe ---
	"أخبار حديثة",
	"الشهر الماضي",
	"اليوم",
	"في 2026",
	"في الآونة الأخيرة",
	"ماذا حدث مؤخرا",
	"مؤخرا",
	"مؤخرًا",
	"هذا الأسبوع",
	"هذا العام",

	// --- Hindi ---
	"2026 में",
	"आज",
	"इन दिनों",
	"इस साल",
	"इस हफ्ते",
	"ताजा खबर",
	"पिछले महीने",
	"हाल ही में",
	"हाल ही में क्या हुआ",

	// --- Japonais ---
	"今年のニュース",
	"今日",
	"今週",
	"先月",
	"最近の出来事",
	"最近何があった",
	"最新ニュース",

	// --- Coréen ---
	"2026년",
	"오늘",
	"올해",
	"요즘",
	"이번 주",
	"지난달",
	"최근",
	"최근에 무슨 일이 있었어",
	"최신 뉴스",

	// --- Turc ---
	"2026 yilinda",
	"2026 yılında",
	"bu gunlerde",
	"bu günlerde",
	"bu hafta",
	"bu yil",
	"bu yıl",
	"bugun",
	"bugün",
	"gecen ay",
	"geçen ay",
	"son haberler",
	"son zamanlarda",
	"son zamanlarda ne oldu",

	// --- Néerlandais ---
	"de laatste tijd",
	"deze week",
	"dit jaar",
	"onlangs",
	"recent nieuws",
	"vandaag",
	"vorige maand",
	"wat is er onlangs gebeurd",

	// --- Polonais ---
	"co sie ostatnio stalo",
	"co się ostatnio stało",
	"dzisiaj",
	"najnowsze wiadomosci",
	"najnowsze wiadomości",
	"ostatnio",
	"w 2026",
	"w ostatnich dniach",
	"w tym roku",
	"w tym tygodniu",
	"w zeszlym miesiacu",
	"w zeszłym miesiącu",

	// --- Vietnamien ---
	"gan day",
	"gan day co gi xay ra",
	"hom nay",
	"nam 2026",
	"nam nay",
	"nhung ngay gan day",
	"thang truoc",
	"tin tuc gan day",
	"tuan nay",
	"gần đây",
	"gần đây có gì xảy ra",
	"hôm nay",
	"năm 2026",
	"năm nay",
	"những ngày gần đây",
	"tháng trước",
	"tin tức gần đây",
	"tuần này",

	// --- Formes identiques dans plusieurs langues (mutualisées pour éviter les
	// doublons, la liste étant comparée indépendamment de la langue détectée) ---
	// « 2026年 »     : chinois simplifié, chinois traditionnel, japonais
	// « 今年 »       : chinois simplifié, chinois traditionnel, japonais
	// « 今天 »       : chinois simplifié, chinois traditionnel, japonais
	// « 最近 »       : chinois simplifié, chinois traditionnel, japonais
	// « 最新消息 »   : chinois simplifié, chinois traditionnel
	// « en 2026 »    : français, espagnol
	// « in 2026 »    : anglais, néerlandais
	// « recentemente » : espagnol, portugais, italien
	// « esta semana »  : espagnol, portugais
	// « este ano »     : espagnol (forme sans accent), portugais
	"2026年",
	"en 2026",
	"esta semana",
	"este ano",
	"in 2026",
	"recentemente",
	"今年",
	"今天",
	"最新消息",
	"最近",
}

// streamingParamKeys : noms de champs JSON liés au streaming que l'on trouve à la
// racine d'un corps de requête. Liste TECHNIQUE, pas linguistique : elle est comparée
// à des clés JSON (map[string]any), pas à du texte. Signal FAIBLE (SigSSEShape), car
// « stream » est le réglage par défaut de tout client en flux.
var streamingParamKeys = []string{
	"continuous_usage_stats",
	"include_usage",
	"stream",
	"stream_include_usage",
	"stream_options",
	"stream_options.include_usage",
}

// suspiciousToolNameFragments : fragments qui DÉSIGNENT une méthode d'empreinte. Un
// outil qui s'appelle ainsi n'a pas d'usage ordinaire dans une application métier :
// c'est un outil de test construit pour interroger le modèle. Signal FORT.
var suspiciousToolNameFragments = []string{
	"benchmark",
	"canary",
	"count_tokens",
	"detect_model",
	"echo_test",
	"fingerprint",
	"get_model",
	"model_info",
	"model_probe",
	"probe_model",
	"token_count",
	"whoami",
}

// genericToolNameFragments : fragments BANALS dans du code réel. « test » est dans
// run_tests, « probe » dans probe_health, « detect » et « ping » dans un outil de
// diagnostic : la production a montré qu'un agent client légitime les déclare. Signal
// CONDITIONNEL (SigToolGenericName) — il ne compte qu'avec une corroboration.
var genericToolNameFragments = []string{
	"detect",
	"dummy",
	"fake",
	"ping",
	"probe",
	"test",
}

// genericToolName : premier fragment banal trouvé dans un nom d'outil ("" si aucun).
func genericToolName(tools []map[string]any) string {
	for _, t := range tools {
		name := strings.ToLower(toolName(t))
		if name == "" {
			continue
		}
		for _, frag := range genericToolNameFragments {
			if frag != "" && strings.Contains(name, frag) {
				return frag
			}
		}
	}
	return ""
}

// toolCountSuspiciousMin : au-delà de ce nombre d'outils déclarés, signal FORT
// (SigToolCountExtreme).
//
// L'hypothèse d'origine (« volume anormal pour un client réel ») a été DÉMENTIE par
// le relevé de production du 2026-09-29 : trois requêtes clientes réelles sur
// deepseek-v4.1-flash ont déclenché `tool_count_extreme` ET `pdf_content` (score
// 8.5, cf. README § fp_observe). Un agent qui attache un PDF et déclare ses outils
// n'a rien d'une sonde. À trancher avant d'activer le reroutage : passer ce signal
// en FAIBLE, ou exiger qu'il soit corroboré. Laissé pour l'instant tel quel — la
// phase 1 n'achemine rien (dry_run), et changer un poids est une décision de
// calibration, pas une correction de bug.
const toolCountSuspiciousMin int = 25

// toolCountNotable : au-delà de ce nombre d'outils, signal FAIBLE (SigToolCount) — les
// agents de codage envoient couramment 10 à 15 outils, donc ce seuil n'est qu'un
// indice destiné à corroborer d'autres signaux.
const toolCountNotable int = 6
