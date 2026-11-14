// Native move of one application dropped on another server of the Placement Board.
export const applicationMoveMessages = {
  en: {
    "applicationMove.summary":
      "{application} moves from {source} to {destination} with its data.",
    "applicationMove.destination": "To",
    "applicationMove.serverOption": "{server} in {stack}",
    "applicationMove.keepSource":
      "This server keeps the data until the other server confirms the move.",
    "applicationMove.start": "Move",
    "applicationMove.starting": "Starting move…",
    "applicationMove.failed":
      "The move could not be accepted. Retry the same attempt.",
    "applicationMove.running": "Moving to the other server…",
    "applicationMove.statusUnavailable":
      "The move status cannot be read right now. Checking again.",
    "applicationMove.outcome.moved":
      "Moved. The application runs on the other server.",
    "applicationMove.outcome.aborted":
      "Not moved. The application runs on this server again.",
    "applicationMove.outcome.destination_verification_required":
      "The other server has not confirmed the move yet. The application stays paused here until it does.",
    "applicationMove.outcome.preflight_failed":
      "The move could not start. Nothing changed.",
    "applicationMove.outcome.failed": "The move did not finish.",
  },
  de: {
    "applicationMove.summary":
      "{application} zieht mit seinen Daten von {source} nach {destination}.",
    "applicationMove.destination": "Nach",
    "applicationMove.serverOption": "{server} in {stack}",
    "applicationMove.keepSource":
      "Dieser Server behält die Daten, bis der andere Server den Umzug bestätigt.",
    "applicationMove.start": "Verschieben",
    "applicationMove.starting": "Umzug startet…",
    "applicationMove.failed":
      "Der Umzug konnte nicht angenommen werden. Wiederhole denselben Versuch.",
    "applicationMove.running": "Wird auf den anderen Server verschoben…",
    "applicationMove.statusUnavailable":
      "Der Status des Umzugs ist gerade nicht lesbar. Wird erneut geprüft.",
    "applicationMove.outcome.moved":
      "Verschoben. Die Anwendung läuft auf dem anderen Server.",
    "applicationMove.outcome.aborted":
      "Nicht verschoben. Die Anwendung läuft wieder auf diesem Server.",
    "applicationMove.outcome.destination_verification_required":
      "Der andere Server hat den Umzug noch nicht bestätigt. Bis dahin bleibt die Anwendung hier angehalten.",
    "applicationMove.outcome.preflight_failed":
      "Der Umzug konnte nicht starten. Es wurde nichts geändert.",
    "applicationMove.outcome.failed": "Der Umzug wurde nicht abgeschlossen.",
  },
  es: {
    "applicationMove.summary":
      "{application} se mueve con sus datos de {source} a {destination}.",
    "applicationMove.destination": "Hacia",
    "applicationMove.serverOption": "{server} en {stack}",
    "applicationMove.keepSource":
      "Este servidor conserva los datos hasta que el otro servidor confirme el traslado.",
    "applicationMove.start": "Mover",
    "applicationMove.starting": "Iniciando traslado…",
    "applicationMove.failed":
      "No se pudo aceptar el traslado. Reintenta el mismo intento.",
    "applicationMove.running": "Moviendo al otro servidor…",
    "applicationMove.statusUnavailable":
      "No se puede leer el estado del traslado ahora. Se comprobará de nuevo.",
    "applicationMove.outcome.moved":
      "Movida. La aplicación se ejecuta en el otro servidor.",
    "applicationMove.outcome.aborted":
      "No se movió. La aplicación vuelve a ejecutarse en este servidor.",
    "applicationMove.outcome.destination_verification_required":
      "El otro servidor aún no ha confirmado el traslado. Hasta entonces, la aplicación sigue en pausa aquí.",
    "applicationMove.outcome.preflight_failed":
      "No se pudo iniciar el traslado. No cambió nada.",
    "applicationMove.outcome.failed": "El traslado no terminó.",
  },
  "zh-Hans": {
    "applicationMove.summary":
      "{application} 将连同其数据从 {source} 迁移到 {destination}。",
    "applicationMove.destination": "到",
    "applicationMove.serverOption": "{stack} 中的 {server}",
    "applicationMove.keepSource":
      "在另一台服务器确认迁移之前，此服务器会保留数据。",
    "applicationMove.start": "迁移",
    "applicationMove.starting": "正在开始迁移…",
    "applicationMove.failed": "无法接受迁移请求。请重试同一次操作。",
    "applicationMove.running": "正在迁移到另一台服务器…",
    "applicationMove.statusUnavailable": "暂时无法读取迁移状态。将再次检查。",
    "applicationMove.outcome.moved": "已迁移。应用正在另一台服务器上运行。",
    "applicationMove.outcome.aborted": "未迁移。应用已在此服务器上重新运行。",
    "applicationMove.outcome.destination_verification_required":
      "另一台服务器尚未确认迁移。在确认之前，应用会在此处保持暂停。",
    "applicationMove.outcome.preflight_failed": "迁移无法开始。未做任何更改。",
    "applicationMove.outcome.failed": "迁移未完成。",
  },
  hi: {
    "applicationMove.summary":
      "{application} अपने डेटा के साथ {source} से {destination} पर जाता है।",
    "applicationMove.destination": "पर",
    "applicationMove.serverOption": "{stack} में {server}",
    "applicationMove.keepSource":
      "जब तक दूसरा सर्वर स्थानांतरण की पुष्टि नहीं करता, यह सर्वर डेटा रखता है।",
    "applicationMove.start": "ले जाएँ",
    "applicationMove.starting": "स्थानांतरण शुरू हो रहा है…",
    "applicationMove.failed":
      "स्थानांतरण स्वीकार नहीं हुआ। वही प्रयास दोबारा करें।",
    "applicationMove.running": "दूसरे सर्वर पर ले जाया जा रहा है…",
    "applicationMove.statusUnavailable":
      "अभी स्थानांतरण की स्थिति पढ़ी नहीं जा सकती। फिर से जाँचा जाएगा।",
    "applicationMove.outcome.moved":
      "स्थानांतरित हो गया। ऐप दूसरे सर्वर पर चल रहा है।",
    "applicationMove.outcome.aborted":
      "स्थानांतरित नहीं हुआ। ऐप फिर से इस सर्वर पर चल रहा है।",
    "applicationMove.outcome.destination_verification_required":
      "दूसरे सर्वर ने अभी तक स्थानांतरण की पुष्टि नहीं की है। तब तक ऐप यहाँ रुका रहेगा।",
    "applicationMove.outcome.preflight_failed":
      "स्थानांतरण शुरू नहीं हो सका। कुछ नहीं बदला।",
    "applicationMove.outcome.failed": "स्थानांतरण पूरा नहीं हुआ।",
  },
  ar: {
    "applicationMove.summary":
      "ينتقل {application} مع بياناته من {source} إلى {destination}.",
    "applicationMove.destination": "إلى",
    "applicationMove.serverOption": "{server} في {stack}",
    "applicationMove.keepSource":
      "يحتفظ هذا الخادم بالبيانات حتى يؤكد الخادم الآخر النقل.",
    "applicationMove.start": "نقل",
    "applicationMove.starting": "جارٍ بدء النقل…",
    "applicationMove.failed": "تعذر قبول طلب النقل. أعد المحاولة نفسها.",
    "applicationMove.running": "جارٍ النقل إلى الخادم الآخر…",
    "applicationMove.statusUnavailable":
      "تتعذر قراءة حالة النقل الآن. سيتم التحقق مجددًا.",
    "applicationMove.outcome.moved": "تم النقل. يعمل التطبيق على الخادم الآخر.",
    "applicationMove.outcome.aborted":
      "لم يتم النقل. يعمل التطبيق على هذا الخادم مرة أخرى.",
    "applicationMove.outcome.destination_verification_required":
      "لم يؤكد الخادم الآخر النقل بعد. يبقى التطبيق متوقفًا مؤقتًا هنا حتى يتم ذلك.",
    "applicationMove.outcome.preflight_failed": "تعذر بدء النقل. لم يتغير شيء.",
    "applicationMove.outcome.failed": "لم يكتمل النقل.",
  },
};
