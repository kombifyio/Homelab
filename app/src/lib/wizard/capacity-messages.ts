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
    "capacity.driver.photosSmartSearch":
      "Smart search in Photos (Immich) adds AI indexing memory.",
    "capacity.driver.photosLite":
      "Immich Lite lowers memory demand for Photos.",
    "capacity.driver.photosEnte":
      "Ente Photos lowers memory demand for Photos and fits Basic Cube S.",
    "capacity.driver.filesPaperless":
      "Adding Paperless-ngx document archive increases memory and storage needs.",
    "capacity.driver.filesNextcloud":
      "Nextcloud uses more memory than the lightweight drive.",
    "capacity.driver.filesOpencloud":
      "OpenCloud adds collaboration services and memory overhead.",
    "capacity.driver.mediaTranscoding":
      "Media transcoding increases CPU and memory load during playback.",
    "capacity.driver.mediaLibrary":
      "Media streaming needs disk space and steady memory for library indexing.",
    "capacity.driver.aiLocalModels":
      "Private AI requires a larger host floor for local models.",
    "capacity.driver.smartHomeHaos":
      "Running Home Assistant OS requires its own virtual machine resources.",
    "capacity.driver.marketplaceRuntime":
      "The app marketplace reserves extra memory for its runtime controller.",
    "capacity.driver.profileLow":
      "Low compute profile reduces baseline resource requirements.",
    "capacity.driver.profileHigh":
      "High compute profile increases headroom and resource reservations.",
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
    "capacity.driver.photosSmartSearch":
      "Intelligente Suche in Fotos (Immich) braucht zusätzlichen KI-Arbeitsspeicher.",
    "capacity.driver.photosLite":
      "Immich Lite senkt den Speicherbedarf für Fotos.",
    "capacity.driver.photosEnte":
      "Ente Photos senkt den Speicherbedarf für Fotos und passt auf Basic Cube S.",
    "capacity.driver.filesPaperless":
      "Das Dokumentenarchiv Paperless-ngx erhöht den Speicher- und Plattenbedarf.",
    "capacity.driver.filesNextcloud":
      "Nextcloud benötigt mehr Arbeitsspeicher als die leichte Dateiablage.",
    "capacity.driver.filesOpencloud":
      "OpenCloud ergänzt Kollaborationsdienste mit zusätzlichem Speicherbedarf.",
    "capacity.driver.mediaTranscoding":
      "Medien-Transkodierung erhöht die CPU- und Speicherlast bei der Wiedergabe.",
    "capacity.driver.mediaLibrary":
      "Medien-Streaming benötigt Speicherplatz und Arbeitsspeicher für die Bibliothek.",
    "capacity.driver.aiLocalModels":
      "Private KI erfordert eine größere Hardware-Basis für lokale Modelle.",
    "capacity.driver.smartHomeHaos":
      "Home Assistant OS benötigt eigene Ressourcen für eine virtuelle Maschine.",
    "capacity.driver.marketplaceRuntime":
      "Der App-Marktplatz reserviert zusätzlichen Arbeitsspeicher für seine Laufzeit.",
    "capacity.driver.profileLow":
      "Das niedrige Leistungsprofil reduziert den Grundbedarf an Ressourcen.",
    "capacity.driver.profileHigh":
      "Das hohe Leistungsprofil erhöht Reserven und Ressourcenbedarf.",
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
    "capacity.driver.photosSmartSearch":
      "La búsqueda inteligente en Fotos (Immich) añade memoria para indexación IA.",
    "capacity.driver.photosLite":
      "Immich Lite reduce la memoria necesaria para Fotos.",
    "capacity.driver.photosEnte":
      "Ente Photos reduce la memoria necesaria para Fotos y cabe en Basic Cube S.",
    "capacity.driver.filesPaperless":
      "Añadir el archivo documental Paperless-ngx aumenta la memoria y el almacenamiento.",
    "capacity.driver.filesNextcloud":
      "Nextcloud consume más memoria que el disco ligero.",
    "capacity.driver.filesOpencloud":
      "OpenCloud añade servicios de colaboración y consumo de memoria.",
    "capacity.driver.mediaTranscoding":
      "La transcodificación multimedia aumenta la carga de CPU y memoria.",
    "capacity.driver.mediaLibrary":
      "El streaming multimedia requiere disco y memoria constante para la biblioteca.",
    "capacity.driver.aiLocalModels":
      "La IA privada requiere un servidor más grande para modelos locales.",
    "capacity.driver.smartHomeHaos":
      "Ejecutar Home Assistant OS requiere recursos propios de máquina virtual.",
    "capacity.driver.marketplaceRuntime":
      "El catálogo de aplicaciones reserva memoria adicional para su controlador.",
    "capacity.driver.profileLow":
      "El perfil de cómputo bajo reduce los recursos base necesarios.",
    "capacity.driver.profileHigh":
      "El perfil de cómputo alto aumenta el margen y las reservas de recursos.",
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
    "capacity.driver.photosSmartSearch":
      "照片（Immich）的智能搜索会增加 AI 索引内存占用。",
    "capacity.driver.photosLite":
      "Immich Lite 可降低照片功能的内存需求。",
    "capacity.driver.photosEnte":
      "Ente Photos 可降低照片功能的内存需求并适配 Basic Cube S。",
    "capacity.driver.filesPaperless":
      "启用 Paperless-ngx 文档归档会增加内存和存储需求。",
    "capacity.driver.filesNextcloud":
      "Nextcloud 比轻量网盘占用更多内存。",
    "capacity.driver.filesOpencloud":
      "OpenCloud 会增加协作服务及内存开销。",
    "capacity.driver.mediaTranscoding":
      "媒体转码会在播放时提高 CPU 与内存负载。",
    "capacity.driver.mediaLibrary":
      "媒体串流需要磁盘空间及稳定的媒体库索引内存。",
    "capacity.driver.aiLocalModels":
      "私有 AI 需要更高配置的主机来运行本地模型。",
    "capacity.driver.smartHomeHaos":
      "运行 Home Assistant OS 需要独立的虚拟机资源。",
    "capacity.driver.marketplaceRuntime":
      "应用市场会为其运行时控制器预留额外内存。",
    "capacity.driver.profileLow":
      "低算力配置会降低基础资源需求。",
    "capacity.driver.profileHigh":
      "高算力配置会提高余量与资源预留。",
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
    "capacity.driver.photosSmartSearch":
      "फ़ोटो (Immich) में स्मार्ट सर्च AI इंडेक्सिंग के लिए अतिरिक्त मेमोरी जोड़ता है।",
    "capacity.driver.photosLite":
      "Immich Lite फ़ोटो के लिए मेमोरी की आवश्यकता कम करता है।",
    "capacity.driver.photosEnte":
      "Ente Photos फ़ोटो के लिए मेमोरी की आवश्यकता कम करता है और Basic Cube S में फिट बैठता है।",
    "capacity.driver.filesPaperless":
      "Paperless-ngx दस्तावेज़ संग्रह जोड़ने से मेमोरी और स्टोरेज की आवश्यकता बढ़ती है।",
    "capacity.driver.filesNextcloud":
      "Nextcloud हल्के ड्राइव की तुलना में अधिक मेमोरी उपयोग करता है।",
    "capacity.driver.filesOpencloud":
      "OpenCloud सहयोग सेवाएँ और अतिरिक्त मेमोरी जोड़ता है।",
    "capacity.driver.mediaTranscoding":
      "मीडिया ट्रांसकोडिंग प्लेबैक के दौरान CPU और मेमोरी लोड बढ़ाता है।",
    "capacity.driver.mediaLibrary":
      "मीडिया स्ट्रीमिंग को लाइब्रेरी इंडेक्सिंग के लिए डिस्क और स्थिर मेमोरी चाहिए।",
    "capacity.driver.aiLocalModels":
      "प्राइवेट AI को लोकल मॉडल के लिए बड़े सर्वर की आवश्यकता होती है।",
    "capacity.driver.smartHomeHaos":
      "Home Assistant OS चलाने के लिए अलग वर्चुअल मशीन संसाधन चाहिए।",
    "capacity.driver.marketplaceRuntime":
      "ऐप मार्केटप्लेस अपने रनटाइम कंट्रोलर के लिए अतिरिक्त मेमोरी आरक्षित करता है।",
    "capacity.driver.profileLow":
      "लो कंप्यूट प्रोफ़ाइल आधारभूत संसाधन आवश्यकता को कम करता है।",
    "capacity.driver.profileHigh":
      "हाई कंप्यूट प्रोफ़ाइल संसाधन आरक्षण और क्षमता बढ़ाता है।",
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
    "capacity.driver.photosSmartSearch":
      "يضيف البحث الذكي في الصور (Immich) ذاكرة لفهرسة الذكاء الاصطناعي.",
    "capacity.driver.photosLite":
      "يخفض Immich Lite استهلاك الذاكرة لتطبيق الصور.",
    "capacity.driver.photosEnte":
      "يخفض Ente Photos استهلاك الذاكرة لتطبيق الصور ويناسب Basic Cube S.",
    "capacity.driver.filesPaperless":
      "إضافة أرشيف المستندات Paperless-ngx تزيد احتياجات الذاكرة والتخزين.",
    "capacity.driver.filesNextcloud":
      "يستهلك Nextcloud ذاكرة أكبر من خدمة الملفات الخفيفة.",
    "capacity.driver.filesOpencloud":
      "يضيف OpenCloud خدمات تعاون واستهلاكاً إضافياً للذاكرة.",
    "capacity.driver.mediaTranscoding":
      "يزيد تحويل الوسائط حمل المعالج والذاكرة أثناء التشغيل.",
    "capacity.driver.mediaLibrary":
      "يحتاج بث الوسائط إلى مساحة قرص وذاكرة ثابتة لفهرسة المكتبة.",
    "capacity.driver.aiLocalModels":
      "يتطلب الذكاء الاصطناعي الخاص خادماً أكبر للنماذج المحلية.",
    "capacity.driver.smartHomeHaos":
      "يتطلب تشغيل Home Assistant OS موارد جهاز افتراضي مستقل.",
    "capacity.driver.marketplaceRuntime":
      "يحجز متجر التطبيقات ذاكرة إضافية لوحدة التحكم الخاصة به.",
    "capacity.driver.profileLow":
      "يقلل ملف الحوسبة المنخفض الاحتياجات الأساسية للموارد.",
    "capacity.driver.profileHigh":
      "يزيد ملف الحوسبة المرتفع هامش الموارد والمحجوزات.",
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
    "capacity.driver.photosSmartSearch":
      "La recherche intelligente dans Photos (Immich) ajoute de la mémoire d'indexation IA.",
    "capacity.driver.photosLite":
      "Immich Lite réduit la mémoire nécessaire pour Photos.",
    "capacity.driver.photosEnte":
      "Ente Photos réduit la mémoire nécessaire pour Photos et convient à Basic Cube S.",
    "capacity.driver.filesPaperless":
      "L'archive documentaire Paperless-ngx augmente les besoins en mémoire et stockage.",
    "capacity.driver.filesNextcloud":
      "Nextcloud consomme plus de mémoire que le disque léger.",
    "capacity.driver.filesOpencloud":
      "OpenCloud ajoute des services collaboratifs et une charge mémoire supplémentaire.",
    "capacity.driver.mediaTranscoding":
      "Le transcodage multimédia augmente la charge CPU et mémoire pendant la lecture.",
    "capacity.driver.mediaLibrary":
      "Le streaming multimédia demande de l'espace disque et de la mémoire pour la médiathèque.",
    "capacity.driver.aiLocalModels":
      "L'IA privée nécessite un serveur plus puissant pour les modèles locaux.",
    "capacity.driver.smartHomeHaos":
      "Home Assistant OS nécessite ses propres ressources de machine virtuelle.",
    "capacity.driver.marketplaceRuntime":
      "La boutique d'applications réserve de la mémoire supplémentaire pour son contrôleur.",
    "capacity.driver.profileLow":
      "Le profil de calcul léger réduit les ressources de base requises.",
    "capacity.driver.profileHigh":
      "Le profil de calcul élevé augmente la marge et les réservations de ressources.",
  },
} as const;
