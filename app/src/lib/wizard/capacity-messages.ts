// Customer-visible strings of the capacity hint (CapacityHint.svelte) and of a
// use case that was not installed for lack of memory (the creation screen).
// Numbers are gigabytes, formatted by the caller. Merged into the main
// dictionary by i18n.ts for every locale; `fr` is the inactive draft catalog
// (locales/fr.pending.ts).
export const capacityMessages = {
  en: {
    "capacity.fits": "Fits · {need} of {have} GB",
    "capacity.notAssessed": "Capacity not assessed",
    "capacity.estimate": "Needs about {need} GB",
    "capacity.estimateAtLeast": "Needs at least {need} GB",
    "capacity.existingShortfall":
      "Existing servers currently provide {existing} GB combined. Choose, add, or deploy another server next.",
    "capacity.tight": "Needs about {need} GB · this server has {have} GB",
    "capacity.tightAtLeast":
      "Needs at least {need} GB · this server has {have} GB",
    "capacity.meter": "Memory: about {need} of {have} GB",
    "capacity.useSize": "Use {size} GB",
    "capacity.leaveOut": "Leave out {useCase}",
    "capacity.rollout.status": "Not installed",
    "capacity.rollout.reason":
      "Not enough memory on this server. The rest is installed and running.",
    "capacity.rollout.reasonNumbers":
      "Needs {need} GB, this server has {have} GB. The rest is installed and running.",
  },
  de: {
    "capacity.fits": "Passt · {need} von {have} GB",
    "capacity.notAssessed": "Kapazität nicht geprüft",
    "capacity.estimate": "Benötigt etwa {need} GB",
    "capacity.estimateAtLeast": "Benötigt mindestens {need} GB",
    "capacity.existingShortfall":
      "Vorhandene Server bieten zusammen {existing} GB. Wähle im nächsten Schritt einen weiteren Server, füge einen hinzu oder stelle einen bereit.",
    "capacity.tight": "Braucht etwa {need} GB · dieser Server hat {have} GB",
    "capacity.tightAtLeast":
      "Braucht mindestens {need} GB · dieser Server hat {have} GB",
    "capacity.meter": "Arbeitsspeicher: etwa {need} von {have} GB",
    "capacity.useSize": "{size} GB nutzen",
    "capacity.leaveOut": "{useCase} weglassen",
    "capacity.rollout.status": "Nicht installiert",
    "capacity.rollout.reason":
      "Zu wenig Arbeitsspeicher auf diesem Server. Alles andere ist installiert und läuft.",
    "capacity.rollout.reasonNumbers":
      "Braucht {need} GB, dieser Server hat {have} GB. Alles andere ist installiert und läuft.",
  },
  es: {
    "capacity.fits": "Cabe · {need} de {have} GB",
    "capacity.notAssessed": "Capacidad sin evaluar",
    "capacity.estimate": "Necesita unos {need} GB",
    "capacity.estimateAtLeast": "Necesita al menos {need} GB",
    "capacity.existingShortfall":
      "Los servidores existentes ofrecen {existing} GB en total. Elige, añade o implementa otro servidor en el siguiente paso.",
    "capacity.tight": "Necesita unos {need} GB · este servidor tiene {have} GB",
    "capacity.tightAtLeast":
      "Necesita al menos {need} GB · este servidor tiene {have} GB",
    "capacity.meter": "Memoria: unos {need} de {have} GB",
    "capacity.useSize": "Usar {size} GB",
    "capacity.leaveOut": "Omitir {useCase}",
    "capacity.rollout.status": "No instalado",
    "capacity.rollout.reason":
      "No hay memoria suficiente en este servidor. Lo demás está instalado y funcionando.",
    "capacity.rollout.reasonNumbers":
      "Necesita {need} GB y este servidor tiene {have} GB. Lo demás está instalado y funcionando.",
  },
  "zh-Hans": {
    "capacity.fits": "够用 · {need} / {have} GB",
    "capacity.notAssessed": "未评估容量",
    "capacity.estimate": "约需 {need} GB",
    "capacity.estimateAtLeast": "至少需要 {need} GB",
    "capacity.existingShortfall":
      "现有服务器合计提供 {existing} GB。请在下一步选择、添加或部署另一台服务器。",
    "capacity.tight": "约需 {need} GB · 此服务器为 {have} GB",
    "capacity.tightAtLeast": "至少需要 {need} GB · 此服务器为 {have} GB",
    "capacity.meter": "内存：约 {need} / {have} GB",
    "capacity.useSize": "使用 {size} GB",
    "capacity.leaveOut": "不安装{useCase}",
    "capacity.rollout.status": "未安装",
    "capacity.rollout.reason": "此服务器内存不足。其余内容已安装并在运行。",
    "capacity.rollout.reasonNumbers":
      "需要 {need} GB，此服务器为 {have} GB。其余内容已安装并在运行。",
  },
  hi: {
    "capacity.fits": "फिट बैठता है · {have} में से {need} GB",
    "capacity.notAssessed": "क्षमता का आकलन नहीं हुआ",
    "capacity.estimate": "लगभग {need} GB चाहिए",
    "capacity.estimateAtLeast": "कम से कम {need} GB चाहिए",
    "capacity.existingShortfall":
      "मौजूदा सर्वर मिलकर {existing} GB देते हैं। अगले चरण में दूसरा सर्वर चुनें, जोड़ें या डिप्लॉय करें।",
    "capacity.tight": "लगभग {need} GB चाहिए · इस सर्वर में {have} GB है",
    "capacity.tightAtLeast":
      "कम से कम {need} GB चाहिए · इस सर्वर में {have} GB है",
    "capacity.meter": "मेमोरी: {have} में से लगभग {need} GB",
    "capacity.useSize": "{size} GB चुनें",
    "capacity.leaveOut": "{useCase} छोड़ें",
    "capacity.rollout.status": "इंस्टॉल नहीं हुआ",
    "capacity.rollout.reason":
      "इस सर्वर पर मेमोरी कम है। बाक़ी सब इंस्टॉल है और चल रहा है।",
    "capacity.rollout.reasonNumbers":
      "{need} GB चाहिए, इस सर्वर में {have} GB है। बाक़ी सब इंस्टॉल है और चल रहा है।",
  },
  ar: {
    "capacity.fits": "مناسب · {need} من {have} غيغابايت",
    "capacity.notAssessed": "لم يتم تقييم السعة",
    "capacity.estimate": "يحتاج نحو {need} غيغابايت",
    "capacity.estimateAtLeast": "يحتاج {need} غيغابايت على الأقل",
    "capacity.existingShortfall":
      "توفر الخوادم الحالية {existing} غيغابايت مجتمعة. اختر خادماً آخر أو أضفه أو انشره في الخطوة التالية.",
    "capacity.tight": "يحتاج نحو {need} غيغابايت · لدى هذا الخادم {have}",
    "capacity.tightAtLeast":
      "يحتاج {need} غيغابايت على الأقل · لدى هذا الخادم {have}",
    "capacity.meter": "الذاكرة: نحو {need} من {have} غيغابايت",
    "capacity.useSize": "استخدام {size} غيغابايت",
    "capacity.leaveOut": "استبعاد {useCase}",
    "capacity.rollout.status": "لم يُثبَّت",
    "capacity.rollout.reason":
      "ذاكرة هذا الخادم غير كافية. كل ما عداه مثبّت ويعمل.",
    "capacity.rollout.reasonNumbers":
      "يحتاج {need} غيغابايت ولدى هذا الخادم {have}. كل ما عداه مثبّت ويعمل.",
  },
  fr: {
    "capacity.fits": "Convient · {need} sur {have} Go",
    "capacity.notAssessed": "Capacité non évaluée",
    "capacity.estimate": "Il faut environ {need} Go",
    "capacity.estimateAtLeast": "Il faut au moins {need} Go",
    "capacity.existingShortfall":
      "Les serveurs existants offrent {existing} Go au total. Choisissez, ajoutez ou déployez un autre serveur à l'étape suivante.",
    "capacity.tight": "Il faut environ {need} Go · ce serveur en a {have}",
    "capacity.tightAtLeast":
      "Il faut au moins {need} Go · ce serveur en a {have}",
    "capacity.meter": "Mémoire : environ {need} sur {have} Go",
    "capacity.useSize": "Utiliser {size} Go",
    "capacity.leaveOut": "Laisser de côté {useCase}",
    "capacity.rollout.status": "Non installé",
    "capacity.rollout.reason":
      "Mémoire insuffisante sur ce serveur. Le reste est installé et fonctionne.",
    "capacity.rollout.reasonNumbers":
      "Il faut {need} Go, ce serveur en a {have}. Le reste est installé et fonctionne.",
  },
} as const;
