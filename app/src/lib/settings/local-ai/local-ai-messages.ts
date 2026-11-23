// Customer-visible strings of Settings › AI providers and the local Companion (workspace
// platform/standards/local-ai-runtimes.v1.md). The card labels match Workbench Desktop's.
// Merged into the main dictionary by i18n.ts.
export const localAiMessages = {
  en: {
    "settings.aiProviders.title": "AI providers",
    "settings.aiProviders.source": "Which AI to use",
    "settings.aiProviders.source.kombify": "kombify AI",
    "settings.aiProviders.source.kombify.description":
      "Through your kombify account.",
    "settings.aiProviders.source.kombify.signedOut":
      "Sign in to kombify to use kombify AI.",
    "settings.aiProviders.source.kombify.own":
      "Your kombify account uses only your own AI.",
    "settings.aiProviders.source.kombify.none":
      "AI is switched off in your kombify account.",
    "settings.aiProviders.source.other": "Other providers",
    "settings.aiProviders.source.other.description":
      "Your own subscriptions and keys.",
    "settings.aiProviders.source.other.unavailable":
      "Homelab Runtime connects no other providers yet.",
    "settings.aiProviders.source.local": "Local",
    "settings.aiProviders.source.local.description":
      "Slower, uses memory and battery.",
    "settings.aiProviders.kombify.description":
      "The Companion answers with kombify AI.",
    "settings.aiProviders.kombify.active": "Active",
    "settings.aiProviders.ollama.description":
      "Shared by the kombify apps on this PC.",
    "settings.aiProviders.endpoint": "Server address",
    "settings.aiProviders.endpoint.description":
      "Ollama server on this machine or your local network.",
    "settings.aiProviders.save": "Save",
    "settings.aiProviders.on": "On",
    "settings.aiProviders.off": "Off",
    "settings.aiProviders.status": "Status",
    "settings.aiProviders.status.checking": "Checking…",
    "settings.aiProviders.status.available": "Available · {models}",
    "settings.aiProviders.status.noModels": "Available, no models yet",
    "settings.aiProviders.status.unreachable": "Not reachable at {endpoint}",
    "settings.aiProviders.refused.endpoint": "Enter an http or https address.",
    "settings.aiProviders.refused.unreadable":
      "The shared file on this PC has another version and stays as it is.",
    "settings.aiProviders.refused.failed": "The setting could not be saved.",
    "settings.aiProviders.thisPc": "{name} (this PC)",
    "settings.aiProviders.remote.description":
      "On your homelab. Shared by the kombify apps on this PC.",
    "settings.aiProviders.remove": "Remove",
    "settings.aiProviders.kombify.localAnswers": "Local AI answers in Homelab",
    "settings.aiProviders.answer": "Answers in Homelab",
    "settings.aiProviders.answer.with": "Companion answers with",
    "settings.aiProviders.answer.description":
      "Applies on this PC to every kombify app that answers from one source, such as Kubi.",
    "settings.aiProviders.answer.kombify": "Your kombify account",
    "settings.aiProviders.answer.local": "Local AI · {name}",
    "settings.aiProviders.answer.account": "Your kombify account's AI answers.",
    "settings.aiProviders.answer.ready": "{name} answers with {model}.",
    "settings.aiProviders.answer.model": "Model",
    "settings.aiProviders.answer.firstModel": "First available model",
    "settings.aiProviders.answer.scope":
      "Your model answers questions and reads the page you are on. Changes to servers and apps run through the kombify Companion.",
    "settings.aiProviders.reason.runtimeMissing":
      "{name} is no longer set up on this PC.",
    "settings.aiProviders.reason.runtimeOff": "{name} is switched off.",
    "settings.aiProviders.reason.unreachable":
      "{name} cannot be reached right now.",
    "settings.aiProviders.reason.noModels": "{name} has no models yet.",
    "settings.aiProviders.reason.modelMissing": "{name} does not have {model}.",
    "settings.aiProviders.reason.noAnswer": "No local answer is chosen.",
    "settings.aiProviders.reason.failed": "{name} stopped answering.",
    "settings.aiProviders.refused.invalid":
      "This runtime is not set up on this PC.",
    "settings.aiProviders.homelab": "Ollama on your homelab",
    "settings.aiProviders.homelab.found": "Ollama on {name}",
    "settings.aiProviders.homelab.add": "Add",
    "settings.aiProviders.homelab.added": "Added",
    "settings.aiProviders.homelab.none":
      "Your servers run no Ollama app yet. Deploy Ollama from the apps, or enter its address below.",
    "settings.aiProviders.homelab.manual": "Add an Ollama server",
    "settings.aiProviders.homelab.description":
      "Your server does the work; this PC sends only the question.",
    "settings.aiProviders.homelab.name": "Name",
    "settings.aiProviders.homelab.namePlaceholder": "e.g. nas",
    "settings.aiProviders.homelab.endpoint.description":
      "Ollama server in your homelab.",
    "localCompanion.title": "Companion",
    "localCompanion.launcher": "Local AI",
    "localCompanion.open": "Open the local Companion",
    "localCompanion.close": "Close",
    "localCompanion.new": "New conversation",
    "localCompanion.source": "Local AI · {name} · {model}",
    "localCompanion.answeredBy": "Answered by {name} · {model}",
    "localCompanion.empty":
      "Ask about what you see here. Your own model answers.",
    "localCompanion.placeholder": "Ask about your homelab…",
    "localCompanion.send": "Send",
    "localCompanion.stop": "Stop",
    "localCompanion.stopped": "Stopped.",
    "localCompanion.thinking": "Thinking…",
    "localCompanion.notSent": "Nothing went to kombify AI.",
    "localCompanion.change": "Change in Settings › AI providers",
  },
  de: {
    "settings.aiProviders.title": "AI-Provider",
    "settings.aiProviders.source": "Welche AI genutzt wird",
    "settings.aiProviders.source.kombify": "kombify AI",
    "settings.aiProviders.source.kombify.description":
      "Über dein kombify-Konto.",
    "settings.aiProviders.source.kombify.signedOut":
      "Melde dich bei kombify an, um kombify AI zu nutzen.",
    "settings.aiProviders.source.kombify.own":
      "Dein kombify-Konto nutzt nur deine eigene AI.",
    "settings.aiProviders.source.kombify.none":
      "AI ist in deinem kombify-Konto ausgeschaltet.",
    "settings.aiProviders.source.other": "Andere Provider",
    "settings.aiProviders.source.other.description":
      "Eigene Abos und Schlüssel.",
    "settings.aiProviders.source.other.unavailable":
      "Homelab Runtime verbindet noch keine anderen Provider.",
    "settings.aiProviders.source.local": "Lokal",
    "settings.aiProviders.source.local.description":
      "Langsamer, braucht Speicher und Akku.",
    "settings.aiProviders.kombify.description":
      "Der Companion antwortet mit kombify AI.",
    "settings.aiProviders.kombify.active": "Aktiv",
    "settings.aiProviders.ollama.description":
      "Gilt für die kombify-Apps auf diesem PC.",
    "settings.aiProviders.endpoint": "Serveradresse",
    "settings.aiProviders.endpoint.description":
      "Ollama-Server auf diesem Rechner oder im lokalen Netz.",
    "settings.aiProviders.save": "Speichern",
    "settings.aiProviders.on": "An",
    "settings.aiProviders.off": "Aus",
    "settings.aiProviders.status": "Status",
    "settings.aiProviders.status.checking": "Wird geprüft…",
    "settings.aiProviders.status.available": "Verfügbar · {models}",
    "settings.aiProviders.status.noModels": "Verfügbar, noch keine Modelle",
    "settings.aiProviders.status.unreachable":
      "Nicht erreichbar unter {endpoint}",
    "settings.aiProviders.refused.endpoint":
      "Gib eine http- oder https-Adresse ein.",
    "settings.aiProviders.refused.unreadable":
      "Die gemeinsame Datei auf diesem PC hat eine andere Version und bleibt unverändert.",
    "settings.aiProviders.refused.failed":
      "Die Einstellung konnte nicht gespeichert werden.",
    "settings.aiProviders.thisPc": "{name} (dieser PC)",
    "settings.aiProviders.remote.description":
      "In deinem Homelab. Gilt für die kombify-Apps auf diesem PC.",
    "settings.aiProviders.remove": "Entfernen",
    "settings.aiProviders.kombify.localAnswers":
      "Lokale AI antwortet in Homelab",
    "settings.aiProviders.answer": "Antworten in Homelab",
    "settings.aiProviders.answer.with": "Companion antwortet mit",
    "settings.aiProviders.answer.description":
      "Gilt auf diesem PC für jede kombify-App, die aus einer Quelle antwortet, etwa Kubi.",
    "settings.aiProviders.answer.kombify": "Deinem kombify-Konto",
    "settings.aiProviders.answer.local": "Lokaler AI · {name}",
    "settings.aiProviders.answer.account":
      "Die AI deines kombify-Kontos antwortet.",
    "settings.aiProviders.answer.ready": "{name} antwortet mit {model}.",
    "settings.aiProviders.answer.model": "Modell",
    "settings.aiProviders.answer.firstModel": "Erstes verfügbares Modell",
    "settings.aiProviders.answer.scope":
      "Dein Modell beantwortet Fragen und liest die Seite, auf der du bist. Änderungen an Servern und Apps laufen über den kombify Companion.",
    "settings.aiProviders.reason.runtimeMissing":
      "{name} ist auf diesem PC nicht mehr eingerichtet.",
    "settings.aiProviders.reason.runtimeOff": "{name} ist ausgeschaltet.",
    "settings.aiProviders.reason.unreachable":
      "{name} ist gerade nicht erreichbar.",
    "settings.aiProviders.reason.noModels": "{name} hat noch keine Modelle.",
    "settings.aiProviders.reason.modelMissing": "{name} hat {model} nicht.",
    "settings.aiProviders.reason.noAnswer": "Keine lokale Antwort gewählt.",
    "settings.aiProviders.reason.failed": "{name} hat aufgehört zu antworten.",
    "settings.aiProviders.refused.invalid":
      "Diese Runtime ist auf diesem PC nicht eingerichtet.",
    "settings.aiProviders.homelab": "Ollama in deinem Homelab",
    "settings.aiProviders.homelab.found": "Ollama auf {name}",
    "settings.aiProviders.homelab.add": "Hinzufügen",
    "settings.aiProviders.homelab.added": "Hinzugefügt",
    "settings.aiProviders.homelab.none":
      "Auf deinen Servern läuft noch keine Ollama-App. Installiere Ollama über die Apps oder gib unten seine Adresse ein.",
    "settings.aiProviders.homelab.manual": "Ollama-Server hinzufügen",
    "settings.aiProviders.homelab.description":
      "Dein Server rechnet; dieser PC schickt nur die Frage.",
    "settings.aiProviders.homelab.name": "Name",
    "settings.aiProviders.homelab.namePlaceholder": "z. B. nas",
    "settings.aiProviders.homelab.endpoint.description":
      "Ollama-Server in deinem Homelab.",
    "localCompanion.title": "Companion",
    "localCompanion.launcher": "Lokale AI",
    "localCompanion.open": "Lokalen Companion öffnen",
    "localCompanion.close": "Schließen",
    "localCompanion.new": "Neues Gespräch",
    "localCompanion.source": "Lokale AI · {name} · {model}",
    "localCompanion.answeredBy": "Antwort von {name} · {model}",
    "localCompanion.empty":
      "Frag nach dem, was du hier siehst. Dein eigenes Modell antwortet.",
    "localCompanion.placeholder": "Frag zu deinem Homelab…",
    "localCompanion.send": "Senden",
    "localCompanion.stop": "Stoppen",
    "localCompanion.stopped": "Gestoppt.",
    "localCompanion.thinking": "Denkt nach…",
    "localCompanion.notSent": "Nichts ging an kombify AI.",
    "localCompanion.change": "In Einstellungen › AI-Provider ändern",
  },
  es: {
    "settings.aiProviders.title": "AI providers",
    "settings.aiProviders.source": "Qué AI usar",
    "settings.aiProviders.source.kombify": "kombify AI",
    "settings.aiProviders.source.kombify.description":
      "Con tu cuenta de kombify.",
    "settings.aiProviders.source.kombify.signedOut":
      "Inicia sesión en kombify para usar kombify AI.",
    "settings.aiProviders.source.kombify.own":
      "Tu cuenta de kombify usa solo tu propia AI.",
    "settings.aiProviders.source.kombify.none":
      "La AI está desactivada en tu cuenta de kombify.",
    "settings.aiProviders.source.other": "Otros provider",
    "settings.aiProviders.source.other.description":
      "Tus suscripciones y claves.",
    "settings.aiProviders.source.other.unavailable":
      "Homelab Runtime aún no conecta otros provider.",
    "settings.aiProviders.source.local": "Local",
    "settings.aiProviders.source.local.description":
      "Más lento, usa memoria y batería.",
    "settings.aiProviders.kombify.description":
      "El Companion responde con kombify AI.",
    "settings.aiProviders.kombify.active": "Activo",
    "settings.aiProviders.ollama.description":
      "Compartido por las apps de kombify en este PC.",
    "settings.aiProviders.endpoint": "Dirección del servidor",
    "settings.aiProviders.endpoint.description":
      "Servidor de Ollama en este equipo o en tu red local.",
    "settings.aiProviders.save": "Guardar",
    "settings.aiProviders.on": "Activado",
    "settings.aiProviders.off": "Desactivado",
    "settings.aiProviders.status": "Estado",
    "settings.aiProviders.status.checking": "Comprobando…",
    "settings.aiProviders.status.available": "Disponible · {models}",
    "settings.aiProviders.status.noModels": "Disponible, aún sin modelos",
    "settings.aiProviders.status.unreachable": "No accesible en {endpoint}",
    "settings.aiProviders.refused.endpoint":
      "Introduce una dirección http o https.",
    "settings.aiProviders.refused.unreadable":
      "El archivo compartido de este PC tiene otra versión y se queda como está.",
    "settings.aiProviders.refused.failed": "No se pudo guardar el ajuste.",
    "settings.aiProviders.thisPc": "{name} (este PC)",
    "settings.aiProviders.remote.description":
      "En tu homelab. Compartido por las apps de kombify en este PC.",
    "settings.aiProviders.remove": "Quitar",
    "settings.aiProviders.kombify.localAnswers":
      "La AI local responde en Homelab",
    "settings.aiProviders.answer": "Respuestas en Homelab",
    "settings.aiProviders.answer.with": "Companion responde con",
    "settings.aiProviders.answer.description":
      "Se aplica en este PC a cada app de kombify que responde desde una fuente, como Kubi.",
    "settings.aiProviders.answer.kombify": "Tu cuenta de kombify",
    "settings.aiProviders.answer.local": "AI local · {name}",
    "settings.aiProviders.answer.account":
      "Responde la AI de tu cuenta de kombify.",
    "settings.aiProviders.answer.ready": "{name} responde con {model}.",
    "settings.aiProviders.answer.model": "Modelo",
    "settings.aiProviders.answer.firstModel": "Primer modelo disponible",
    "settings.aiProviders.answer.scope":
      "Tu modelo responde preguntas y lee la página en la que estás. Los cambios en servidores y apps pasan por el kombify Companion.",
    "settings.aiProviders.reason.runtimeMissing":
      "{name} ya no está configurado en este PC.",
    "settings.aiProviders.reason.runtimeOff": "{name} está desactivado.",
    "settings.aiProviders.reason.unreachable":
      "{name} no está accesible ahora.",
    "settings.aiProviders.reason.noModels": "{name} aún no tiene modelos.",
    "settings.aiProviders.reason.modelMissing": "{name} no tiene {model}.",
    "settings.aiProviders.reason.noAnswer": "No hay respuesta local elegida.",
    "settings.aiProviders.reason.failed": "{name} dejó de responder.",
    "settings.aiProviders.refused.invalid":
      "Este runtime no está configurado en este PC.",
    "settings.aiProviders.homelab": "Ollama en tu homelab",
    "settings.aiProviders.homelab.found": "Ollama en {name}",
    "settings.aiProviders.homelab.add": "Añadir",
    "settings.aiProviders.homelab.added": "Añadido",
    "settings.aiProviders.homelab.none":
      "Tus servidores aún no ejecutan la app Ollama. Despliega Ollama desde las apps o escribe su dirección abajo.",
    "settings.aiProviders.homelab.manual": "Añadir un servidor Ollama",
    "settings.aiProviders.homelab.description":
      "Tu servidor hace el trabajo; este PC solo envía la pregunta.",
    "settings.aiProviders.homelab.name": "Nombre",
    "settings.aiProviders.homelab.namePlaceholder": "p. ej. nas",
    "settings.aiProviders.homelab.endpoint.description":
      "Servidor Ollama en tu homelab.",
    "localCompanion.title": "Companion",
    "localCompanion.launcher": "AI local",
    "localCompanion.open": "Abrir el Companion local",
    "localCompanion.close": "Cerrar",
    "localCompanion.new": "Nueva conversación",
    "localCompanion.source": "AI local · {name} · {model}",
    "localCompanion.answeredBy": "Respondido por {name} · {model}",
    "localCompanion.empty":
      "Pregunta por lo que ves aquí. Responde tu propio modelo.",
    "localCompanion.placeholder": "Pregunta sobre tu homelab…",
    "localCompanion.send": "Enviar",
    "localCompanion.stop": "Detener",
    "localCompanion.stopped": "Detenido.",
    "localCompanion.thinking": "Pensando…",
    "localCompanion.notSent": "Nada se envió a kombify AI.",
    "localCompanion.change": "Cambiar en Ajustes › AI providers",
  },
  "zh-Hans": {
    "settings.aiProviders.title": "AI Provider",
    "settings.aiProviders.source": "使用哪种 AI",
    "settings.aiProviders.source.kombify": "kombify AI",
    "settings.aiProviders.source.kombify.description":
      "通过你的 kombify 账户。",
    "settings.aiProviders.source.kombify.signedOut":
      "登录 kombify 即可使用 kombify AI。",
    "settings.aiProviders.source.kombify.own":
      "你的 kombify 账户只使用你自己的 AI。",
    "settings.aiProviders.source.kombify.none": "你的 kombify 账户已关闭 AI。",
    "settings.aiProviders.source.other": "其他 Provider",
    "settings.aiProviders.source.other.description": "你自己的订阅和密钥。",
    "settings.aiProviders.source.other.unavailable":
      "Homelab Runtime 尚未连接其他 Provider。",
    "settings.aiProviders.source.local": "本地",
    "settings.aiProviders.source.local.description": "较慢，占用内存和电量。",
    "settings.aiProviders.kombify.description":
      "Companion 使用 kombify AI 回答。",
    "settings.aiProviders.kombify.active": "已启用",
    "settings.aiProviders.ollama.description":
      "由此电脑上的 kombify 应用共享。",
    "settings.aiProviders.endpoint": "服务器地址",
    "settings.aiProviders.endpoint.description":
      "此电脑或本地网络中的 Ollama 服务器。",
    "settings.aiProviders.save": "保存",
    "settings.aiProviders.on": "开",
    "settings.aiProviders.off": "关",
    "settings.aiProviders.status": "状态",
    "settings.aiProviders.status.checking": "正在检查…",
    "settings.aiProviders.status.available": "可用 · {models}",
    "settings.aiProviders.status.noModels": "可用，尚无模型",
    "settings.aiProviders.status.unreachable": "无法通过 {endpoint} 访问",
    "settings.aiProviders.refused.endpoint": "请输入 http 或 https 地址。",
    "settings.aiProviders.refused.unreadable":
      "此电脑上的共享文件版本不同，将保持不变。",
    "settings.aiProviders.refused.failed": "无法保存此设置。",
    "settings.aiProviders.thisPc": "{name}（此电脑）",
    "settings.aiProviders.remote.description":
      "位于你的 homelab。由此电脑上的 kombify 应用共享。",
    "settings.aiProviders.remove": "移除",
    "settings.aiProviders.kombify.localAnswers": "本地 AI 在 Homelab 中回答",
    "settings.aiProviders.answer": "Homelab 中的回答",
    "settings.aiProviders.answer.with": "Companion 回答来源",
    "settings.aiProviders.answer.description":
      "适用于此电脑上所有从单一来源回答的 kombify 应用，例如 Kubi。",
    "settings.aiProviders.answer.kombify": "你的 kombify 账户",
    "settings.aiProviders.answer.local": "本地 AI · {name}",
    "settings.aiProviders.answer.account": "由你的 kombify 账户的 AI 回答。",
    "settings.aiProviders.answer.ready": "{name} 使用 {model} 回答。",
    "settings.aiProviders.answer.model": "模型",
    "settings.aiProviders.answer.firstModel": "第一个可用模型",
    "settings.aiProviders.answer.scope":
      "你的模型回答问题并读取你所在的页面。对服务器和应用的更改通过 kombify Companion 进行。",
    "settings.aiProviders.reason.runtimeMissing": "{name} 已不在此电脑上设置。",
    "settings.aiProviders.reason.runtimeOff": "{name} 已关闭。",
    "settings.aiProviders.reason.unreachable": "{name} 当前无法访问。",
    "settings.aiProviders.reason.noModels": "{name} 还没有模型。",
    "settings.aiProviders.reason.modelMissing": "{name} 没有 {model}。",
    "settings.aiProviders.reason.noAnswer": "尚未选择本地回答。",
    "settings.aiProviders.reason.failed": "{name} 停止了回答。",
    "settings.aiProviders.refused.invalid": "此运行时未在此电脑上设置。",
    "settings.aiProviders.homelab": "你 homelab 中的 Ollama",
    "settings.aiProviders.homelab.found": "{name} 上的 Ollama",
    "settings.aiProviders.homelab.add": "添加",
    "settings.aiProviders.homelab.added": "已添加",
    "settings.aiProviders.homelab.none":
      "你的服务器尚未运行 Ollama 应用。可从应用中部署 Ollama，或在下方输入其地址。",
    "settings.aiProviders.homelab.manual": "添加 Ollama 服务器",
    "settings.aiProviders.homelab.description":
      "由你的服务器完成计算；此电脑只发送问题。",
    "settings.aiProviders.homelab.name": "名称",
    "settings.aiProviders.homelab.namePlaceholder": "例如 nas",
    "settings.aiProviders.homelab.endpoint.description":
      "你 homelab 中的 Ollama 服务器。",
    "localCompanion.title": "Companion",
    "localCompanion.launcher": "本地 AI",
    "localCompanion.open": "打开本地 Companion",
    "localCompanion.close": "关闭",
    "localCompanion.new": "新对话",
    "localCompanion.source": "本地 AI · {name} · {model}",
    "localCompanion.answeredBy": "由 {name} · {model} 回答",
    "localCompanion.empty": "询问你在这里看到的内容。由你自己的模型回答。",
    "localCompanion.placeholder": "询问你的 homelab…",
    "localCompanion.send": "发送",
    "localCompanion.stop": "停止",
    "localCompanion.stopped": "已停止。",
    "localCompanion.thinking": "思考中…",
    "localCompanion.notSent": "没有内容发送到 kombify AI。",
    "localCompanion.change": "在设置 › AI Provider 中更改",
  },
  hi: {
    "settings.aiProviders.title": "AI Provider",
    "settings.aiProviders.source": "कौन-सा AI उपयोग करें",
    "settings.aiProviders.source.kombify": "kombify AI",
    "settings.aiProviders.source.kombify.description": "आपके kombify खाते से।",
    "settings.aiProviders.source.kombify.signedOut":
      "kombify AI उपयोग करने के लिए kombify में साइन इन करें।",
    "settings.aiProviders.source.kombify.own":
      "आपका kombify खाता केवल आपका अपना AI उपयोग करता है।",
    "settings.aiProviders.source.kombify.none":
      "आपके kombify खाते में AI बंद है।",
    "settings.aiProviders.source.other": "अन्य Provider",
    "settings.aiProviders.source.other.description":
      "आपकी सदस्यताएँ और कुंजियाँ।",
    "settings.aiProviders.source.other.unavailable":
      "Homelab Runtime अभी कोई अन्य Provider नहीं जोड़ता।",
    "settings.aiProviders.source.local": "लोकल",
    "settings.aiProviders.source.local.description":
      "धीमा, मेमोरी व बैटरी खर्च करता है।",
    "settings.aiProviders.kombify.description":
      "Companion kombify AI से जवाब देता है।",
    "settings.aiProviders.kombify.active": "सक्रिय",
    "settings.aiProviders.ollama.description":
      "इस PC के kombify ऐप्स में साझा।",
    "settings.aiProviders.endpoint": "सर्वर पता",
    "settings.aiProviders.endpoint.description":
      "इस मशीन या आपके लोकल नेटवर्क पर Ollama सर्वर।",
    "settings.aiProviders.save": "सहेजें",
    "settings.aiProviders.on": "चालू",
    "settings.aiProviders.off": "बंद",
    "settings.aiProviders.status": "स्थिति",
    "settings.aiProviders.status.checking": "जाँच हो रही है…",
    "settings.aiProviders.status.available": "उपलब्ध · {models}",
    "settings.aiProviders.status.noModels": "उपलब्ध, अभी कोई मॉडल नहीं",
    "settings.aiProviders.status.unreachable": "{endpoint} पर पहुँच नहीं",
    "settings.aiProviders.refused.endpoint": "http या https पता दर्ज करें।",
    "settings.aiProviders.refused.unreadable":
      "इस PC की साझा फ़ाइल का संस्करण अलग है, वह जैसी है वैसी रहेगी।",
    "settings.aiProviders.refused.failed": "सेटिंग सहेजी नहीं जा सकी।",
    "settings.aiProviders.thisPc": "{name} (यह PC)",
    "settings.aiProviders.remote.description":
      "आपके homelab में। इस PC के kombify ऐप्स में साझा।",
    "settings.aiProviders.remove": "हटाएँ",
    "settings.aiProviders.kombify.localAnswers":
      "Homelab में लोकल AI जवाब देता है",
    "settings.aiProviders.answer": "Homelab में जवाब",
    "settings.aiProviders.answer.with": "Companion इससे जवाब देता है",
    "settings.aiProviders.answer.description":
      "इस PC पर हर उस kombify ऐप पर लागू जो एक स्रोत से जवाब देता है, जैसे Kubi।",
    "settings.aiProviders.answer.kombify": "आपका kombify खाता",
    "settings.aiProviders.answer.local": "लोकल AI · {name}",
    "settings.aiProviders.answer.account":
      "आपके kombify खाते का AI जवाब देता है।",
    "settings.aiProviders.answer.ready": "{name} {model} से जवाब देता है।",
    "settings.aiProviders.answer.model": "मॉडल",
    "settings.aiProviders.answer.firstModel": "पहला उपलब्ध मॉडल",
    "settings.aiProviders.answer.scope":
      "आपका मॉडल सवालों के जवाब देता है और वह पेज पढ़ता है जिस पर आप हैं। सर्वर और ऐप्स में बदलाव kombify Companion से होते हैं।",
    "settings.aiProviders.reason.runtimeMissing":
      "{name} अब इस PC पर सेट नहीं है।",
    "settings.aiProviders.reason.runtimeOff": "{name} बंद है।",
    "settings.aiProviders.reason.unreachable": "{name} अभी पहुँच में नहीं है।",
    "settings.aiProviders.reason.noModels": "{name} में अभी कोई मॉडल नहीं है।",
    "settings.aiProviders.reason.modelMissing": "{name} में {model} नहीं है।",
    "settings.aiProviders.reason.noAnswer": "कोई लोकल जवाब चुना नहीं गया है।",
    "settings.aiProviders.reason.failed": "{name} ने जवाब देना बंद कर दिया।",
    "settings.aiProviders.refused.invalid": "यह runtime इस PC पर सेट नहीं है।",
    "settings.aiProviders.homelab": "आपके homelab में Ollama",
    "settings.aiProviders.homelab.found": "{name} पर Ollama",
    "settings.aiProviders.homelab.add": "जोड़ें",
    "settings.aiProviders.homelab.added": "जोड़ा गया",
    "settings.aiProviders.homelab.none":
      "आपके सर्वर पर अभी कोई Ollama ऐप नहीं चलता। ऐप्स से Ollama डिप्लॉय करें, या नीचे उसका पता डालें।",
    "settings.aiProviders.homelab.manual": "Ollama सर्वर जोड़ें",
    "settings.aiProviders.homelab.description":
      "काम आपका सर्वर करता है; यह PC सिर्फ़ सवाल भेजता है।",
    "settings.aiProviders.homelab.name": "नाम",
    "settings.aiProviders.homelab.namePlaceholder": "जैसे nas",
    "settings.aiProviders.homelab.endpoint.description":
      "आपके homelab में Ollama सर्वर।",
    "localCompanion.title": "Companion",
    "localCompanion.launcher": "लोकल AI",
    "localCompanion.open": "लोकल Companion खोलें",
    "localCompanion.close": "बंद करें",
    "localCompanion.new": "नई बातचीत",
    "localCompanion.source": "लोकल AI · {name} · {model}",
    "localCompanion.answeredBy": "{name} · {model} का जवाब",
    "localCompanion.empty":
      "यहाँ जो दिखता है उसके बारे में पूछें। आपका अपना मॉडल जवाब देता है।",
    "localCompanion.placeholder": "अपने homelab के बारे में पूछें…",
    "localCompanion.send": "भेजें",
    "localCompanion.stop": "रोकें",
    "localCompanion.stopped": "रोका गया।",
    "localCompanion.thinking": "सोच रहा है…",
    "localCompanion.notSent": "kombify AI को कुछ नहीं भेजा गया।",
    "localCompanion.change": "सेटिंग्स › AI Provider में बदलें",
  },
  ar: {
    "settings.aiProviders.title": "AI Provider",
    "settings.aiProviders.source": "أي AI يُستخدم",
    "settings.aiProviders.source.kombify": "kombify AI",
    "settings.aiProviders.source.kombify.description": "عبر حسابك في kombify.",
    "settings.aiProviders.source.kombify.signedOut":
      "سجّل الدخول إلى kombify لاستخدام kombify AI.",
    "settings.aiProviders.source.kombify.own":
      "حسابك في kombify يستخدم AI الخاص بك فقط.",
    "settings.aiProviders.source.kombify.none": "AI متوقف في حسابك في kombify.",
    "settings.aiProviders.source.other": "Provider آخرون",
    "settings.aiProviders.source.other.description":
      "اشتراكاتك ومفاتيحك الخاصة.",
    "settings.aiProviders.source.other.unavailable":
      "لا يربط Homelab Runtime أي Provider آخر بعد.",
    "settings.aiProviders.source.local": "محلي",
    "settings.aiProviders.source.local.description":
      "أبطأ، ويستهلك الذاكرة والبطارية.",
    "settings.aiProviders.kombify.description":
      "يجيب Companion باستخدام kombify AI.",
    "settings.aiProviders.kombify.active": "نشط",
    "settings.aiProviders.ollama.description":
      "مشترك بين تطبيقات kombify على هذا الجهاز.",
    "settings.aiProviders.endpoint": "عنوان الخادم",
    "settings.aiProviders.endpoint.description":
      "خادم Ollama على هذا الجهاز أو في شبكتك المحلية.",
    "settings.aiProviders.save": "حفظ",
    "settings.aiProviders.on": "تشغيل",
    "settings.aiProviders.off": "إيقاف",
    "settings.aiProviders.status": "الحالة",
    "settings.aiProviders.status.checking": "جارٍ التحقق…",
    "settings.aiProviders.status.available": "متاح · {models}",
    "settings.aiProviders.status.noModels": "متاح، لا توجد نماذج بعد",
    "settings.aiProviders.status.unreachable": "غير قابل للوصول على {endpoint}",
    "settings.aiProviders.refused.endpoint": "أدخل عنوان http أو https.",
    "settings.aiProviders.refused.unreadable":
      "الملف المشترك على هذا الجهاز بإصدار آخر ويبقى كما هو.",
    "settings.aiProviders.refused.failed": "تعذّر حفظ الإعداد.",
    "settings.aiProviders.thisPc": "{name} (هذا الجهاز)",
    "settings.aiProviders.remote.description":
      "في الـ homelab الخاص بك. مشترك بين تطبيقات kombify على هذا الجهاز.",
    "settings.aiProviders.remove": "إزالة",
    "settings.aiProviders.kombify.localAnswers":
      "الـ AI المحلي يجيب في Homelab",
    "settings.aiProviders.answer": "الإجابات في Homelab",
    "settings.aiProviders.answer.with": "يجيب Companion عبر",
    "settings.aiProviders.answer.description":
      "ينطبق على هذا الجهاز على كل تطبيق kombify يجيب من مصدر واحد، مثل Kubi.",
    "settings.aiProviders.answer.kombify": "حساب kombify الخاص بك",
    "settings.aiProviders.answer.local": "AI محلي · {name}",
    "settings.aiProviders.answer.account": "يجيب الـ AI الخاص بحساب kombify.",
    "settings.aiProviders.answer.ready": "{name} يجيب باستخدام {model}.",
    "settings.aiProviders.answer.model": "النموذج",
    "settings.aiProviders.answer.firstModel": "أول نموذج متاح",
    "settings.aiProviders.answer.scope":
      "نموذجك يجيب عن الأسئلة ويقرأ الصفحة التي أنت عليها. التغييرات على الخوادم والتطبيقات تتم عبر kombify Companion.",
    "settings.aiProviders.reason.runtimeMissing":
      "{name} لم يعد معدًّا على هذا الجهاز.",
    "settings.aiProviders.reason.runtimeOff": "{name} متوقف.",
    "settings.aiProviders.reason.unreachable":
      "تعذّر الوصول إلى {name} حاليًا.",
    "settings.aiProviders.reason.noModels": "لا توجد نماذج في {name} بعد.",
    "settings.aiProviders.reason.modelMissing": "{name} لا يحتوي على {model}.",
    "settings.aiProviders.reason.noAnswer": "لم يتم اختيار إجابة محلية.",
    "settings.aiProviders.reason.failed": "توقف {name} عن الإجابة.",
    "settings.aiProviders.refused.invalid":
      "هذا الـ runtime غير معدّ على هذا الجهاز.",
    "settings.aiProviders.homelab": "Ollama في الـ homelab الخاص بك",
    "settings.aiProviders.homelab.found": "Ollama على {name}",
    "settings.aiProviders.homelab.add": "إضافة",
    "settings.aiProviders.homelab.added": "تمت الإضافة",
    "settings.aiProviders.homelab.none":
      "لا يشغّل أي من خوادمك تطبيق Ollama بعد. انشر Ollama من التطبيقات أو أدخل عنوانه أدناه.",
    "settings.aiProviders.homelab.manual": "إضافة خادم Ollama",
    "settings.aiProviders.homelab.description":
      "خادمك يقوم بالعمل؛ هذا الجهاز يرسل السؤال فقط.",
    "settings.aiProviders.homelab.name": "الاسم",
    "settings.aiProviders.homelab.namePlaceholder": "مثل nas",
    "settings.aiProviders.homelab.endpoint.description":
      "خادم Ollama في الـ homelab الخاص بك.",
    "localCompanion.title": "Companion",
    "localCompanion.launcher": "AI محلي",
    "localCompanion.open": "فتح Companion المحلي",
    "localCompanion.close": "إغلاق",
    "localCompanion.new": "محادثة جديدة",
    "localCompanion.source": "AI محلي · {name} · {model}",
    "localCompanion.answeredBy": "أجاب {name} · {model}",
    "localCompanion.empty": "اسأل عمّا تراه هنا. يجيب نموذجك الخاص.",
    "localCompanion.placeholder": "اسأل عن الـ homelab الخاص بك…",
    "localCompanion.send": "إرسال",
    "localCompanion.stop": "إيقاف",
    "localCompanion.stopped": "تم الإيقاف.",
    "localCompanion.thinking": "يفكر…",
    "localCompanion.notSent": "لم يُرسل شيء إلى kombify AI.",
    "localCompanion.change": "غيّر ذلك في الإعدادات › AI Provider",
  },
  // Draft for the inactive French catalog (locales/fr.pending.ts).
  fr: {
    "settings.aiProviders.title": "Fournisseurs d’IA",
    "settings.aiProviders.source": "Quelle IA utiliser",
    "settings.aiProviders.source.kombify": "kombify AI",
    "settings.aiProviders.source.kombify.description":
      "Avec ton compte kombify.",
    "settings.aiProviders.source.kombify.signedOut":
      "Connecte-toi à kombify pour utiliser kombify AI.",
    "settings.aiProviders.source.kombify.own":
      "Ton compte kombify utilise uniquement ta propre IA.",
    "settings.aiProviders.source.kombify.none":
      "L’IA est désactivée dans ton compte kombify.",
    "settings.aiProviders.source.other": "Autres fournisseurs",
    "settings.aiProviders.source.other.description":
      "Tes propres abonnements et clés.",
    "settings.aiProviders.source.other.unavailable":
      "Homelab Runtime ne connecte pas encore d’autres fournisseurs.",
    "settings.aiProviders.source.local": "Local",
    "settings.aiProviders.source.local.description":
      "Plus lent, utilise mémoire et batterie.",
    "settings.aiProviders.kombify.description":
      "Le Companion répond avec kombify AI.",
    "settings.aiProviders.kombify.active": "Actif",
    "settings.aiProviders.ollama.description":
      "Partagé par les apps kombify de ce PC.",
    "settings.aiProviders.endpoint": "Adresse du serveur",
    "settings.aiProviders.endpoint.description":
      "Serveur Ollama sur cette machine ou ton réseau local.",
    "settings.aiProviders.save": "Enregistrer",
    "settings.aiProviders.on": "Activé",
    "settings.aiProviders.off": "Désactivé",
    "settings.aiProviders.status": "État",
    "settings.aiProviders.status.checking": "Vérification…",
    "settings.aiProviders.status.available": "Disponible · {models}",
    "settings.aiProviders.status.noModels": "Disponible, pas encore de modèles",
    "settings.aiProviders.status.unreachable": "Injoignable à {endpoint}",
    "settings.aiProviders.refused.endpoint":
      "Saisis une adresse http ou https.",
    "settings.aiProviders.refused.unreadable":
      "Le fichier partagé de ce PC a une autre version et reste tel quel.",
    "settings.aiProviders.refused.failed":
      "Le réglage n’a pas pu être enregistré.",
    "settings.aiProviders.thisPc": "{name} (ce PC)",
    "settings.aiProviders.remote.description":
      "Dans ton homelab. Partagé par les apps kombify de ce PC.",
    "settings.aiProviders.remove": "Retirer",
    "settings.aiProviders.kombify.localAnswers":
      "L’IA locale répond dans Homelab",
    "settings.aiProviders.answer": "Réponses dans Homelab",
    "settings.aiProviders.answer.with": "Le Companion répond avec",
    "settings.aiProviders.answer.description":
      "S’applique sur ce PC à chaque app kombify qui répond depuis une source, comme Kubi.",
    "settings.aiProviders.answer.kombify": "Ton compte kombify",
    "settings.aiProviders.answer.local": "IA locale · {name}",
    "settings.aiProviders.answer.account": "L’IA de ton compte kombify répond.",
    "settings.aiProviders.answer.ready": "{name} répond avec {model}.",
    "settings.aiProviders.answer.model": "Modèle",
    "settings.aiProviders.answer.firstModel": "Premier modèle disponible",
    "settings.aiProviders.answer.scope":
      "Ton modèle répond aux questions et lit la page où tu es. Les modifications de serveurs et d’apps passent par le kombify Companion.",
    "settings.aiProviders.reason.runtimeMissing":
      "{name} n’est plus configuré sur ce PC.",
    "settings.aiProviders.reason.runtimeOff": "{name} est désactivé.",
    "settings.aiProviders.reason.unreachable":
      "{name} est injoignable pour le moment.",
    "settings.aiProviders.reason.noModels": "{name} n’a pas encore de modèles.",
    "settings.aiProviders.reason.modelMissing": "{name} n’a pas {model}.",
    "settings.aiProviders.reason.noAnswer":
      "Aucune réponse locale n’est choisie.",
    "settings.aiProviders.reason.failed": "{name} a cessé de répondre.",
    "settings.aiProviders.refused.invalid":
      "Ce runtime n’est pas configuré sur ce PC.",
    "settings.aiProviders.homelab": "Ollama dans ton homelab",
    "settings.aiProviders.homelab.found": "Ollama sur {name}",
    "settings.aiProviders.homelab.add": "Ajouter",
    "settings.aiProviders.homelab.added": "Ajouté",
    "settings.aiProviders.homelab.none":
      "Tes serveurs n’exécutent pas encore l’app Ollama. Déploie Ollama depuis les apps ou saisis son adresse ci-dessous.",
    "settings.aiProviders.homelab.manual": "Ajouter un serveur Ollama",
    "settings.aiProviders.homelab.description":
      "Ton serveur fait le travail ; ce PC envoie seulement la question.",
    "settings.aiProviders.homelab.name": "Nom",
    "settings.aiProviders.homelab.namePlaceholder": "p. ex. nas",
    "settings.aiProviders.homelab.endpoint.description":
      "Serveur Ollama dans ton homelab.",
    "localCompanion.title": "Companion",
    "localCompanion.launcher": "IA locale",
    "localCompanion.open": "Ouvrir le Companion local",
    "localCompanion.close": "Fermer",
    "localCompanion.new": "Nouvelle conversation",
    "localCompanion.source": "IA locale · {name} · {model}",
    "localCompanion.answeredBy": "Réponse de {name} · {model}",
    "localCompanion.empty":
      "Pose une question sur ce que tu vois ici. Ton propre modèle répond.",
    "localCompanion.placeholder": "Pose une question sur ton homelab…",
    "localCompanion.send": "Envoyer",
    "localCompanion.stop": "Arrêter",
    "localCompanion.stopped": "Arrêté.",
    "localCompanion.thinking": "Réflexion…",
    "localCompanion.notSent": "Rien n’est parti vers kombify AI.",
    "localCompanion.change": "Modifier dans Réglages › Fournisseurs d’IA",
  },
} as const;
